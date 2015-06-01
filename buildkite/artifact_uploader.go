package buildkite

import (
	"crypto/sha1"
	"errors"
	"fmt"
	"github.com/buildkite/agent/buildkite/glob"
	"github.com/buildkite/agent/buildkite/logger"
	"github.com/buildkite/agent/buildkite/pool"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type ArtifactUploader struct {
	// The ID of the Job
	JobID string

	// The path of the uploads
	Paths string

	// Where we'll be uploading artifacts
	Destination string

	// The API used for communication
	API API
}

func (a *ArtifactUploader) Upload() error {
	// Create artifact structs for all the files we need to upload
	artifacts, err := a.collect()
	if err != nil {
		return err
	}

	if len(artifacts) == 0 {
		logger.Info("No files matched paths: %s", a.Paths)
	} else {
		logger.Info("Found %d files that match \"%s\"", len(artifacts), a.Paths)

		err := a.upload(artifacts)
		if err != nil {
			return err
		}
	}

	return nil
}

func (a *ArtifactUploader) collect() (artifacts []*Artifact, err error) {
	globPaths := strings.Split(a.Paths, ";")
	workingDirectory, _ := os.Getwd()

	for _, globPath := range globPaths {
		globPath = strings.TrimSpace(globPath)

		if globPath != "" {
			logger.Debug("Globbing %s for %s", workingDirectory, globPath)

			files, err := glob.Glob(workingDirectory, globPath)
			if err != nil {
				return nil, err
			}

			for _, file := range files {
				// Generate an absolute path for the artifact
				absolutePath, err := filepath.Abs(file)
				if err != nil {
					return nil, err
				}

				fileInfo, err := os.Stat(absolutePath)
				if fileInfo.IsDir() {
					logger.Debug("Skipping directory %s", file)
					continue
				}

				// Create a relative path (from the workingDirectory) to the artifact, by removing the
				// first part of the absolutePath that is the workingDirectory.
				relativePath := strings.Replace(absolutePath, workingDirectory, "", 1)

				// Ensure the relativePath doesn't have a file seperator "/" as the first character
				relativePath = strings.TrimPrefix(relativePath, string(os.PathSeparator))

				// Build an artifact object using the paths we have.
				artifact, err := a.build(relativePath, absolutePath, globPath)
				if err != nil {
					return nil, err
				}

				artifacts = append(artifacts, artifact)
			}
		}
	}

	return artifacts, nil
}

func (a *ArtifactUploader) build(relativePath string, absolutePath string, globPath string) (*Artifact, error) {
	// Temporarily open the file to get it's size
	file, err := os.Open(absolutePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Grab it's file info (which includes it's file size)
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}

	// Generate a sha1 checksum for the file
	hash := sha1.New()
	io.Copy(hash, file)
	checksum := fmt.Sprintf("%x", hash.Sum(nil))

	// Create our new artifact data structure
	artifact := new(Artifact)
	artifact.API = a.API
	artifact.JobID = a.JobID
	artifact.State = "new"
	artifact.Path = relativePath
	artifact.AbsolutePath = absolutePath
	artifact.GlobPath = globPath
	artifact.FileSize = fileInfo.Size()
	artifact.Sha1Sum = checksum

	return artifact, nil
}

func (a *ArtifactUploader) upload(artifacts []*Artifact) error {
	var uploader Uploader

	// Determine what uploader to use
	if a.Destination != "" {
		if strings.HasPrefix(a.Destination, "s3://") {
			uploader = new(S3Uploader)
		} else {
			return errors.New("Unknown upload destination: " + a.Destination)
		}
	} else {
		uploader = new(FormUploader)
	}

	// Setup the uploader
	err := uploader.Setup(a.Destination)
	if err != nil {
		return err
	}

	// Set the URL's of the artifacts based on the uploader
	for _, artifact := range artifacts {
		artifact.URL = uploader.URL(artifact)
	}

	// Create the artifacts on Buildkite
	batchCreator := ArtifactBatchCreator{
		API:       a.API,
		JobID:     a.JobID,
		Artifacts: artifacts,
	}
	err = batchCreator.Create()
	if err != nil {
		return err
	}

	p := pool.New(pool.MaxConcurrencyLimit)
	errors := []error{}

	for _, artifact := range artifacts {
		// Create new instance of the artifact for the goroutine
		// See: http://golang.org/doc/effective_go.html#channels
		artifact := artifact

		p.Spawn(func() {
			// Show a nice message that we're starting to upload the file
			logger.Info("Uploading \"%s\" %d bytes", artifact.Path, artifact.FileSize)

			// Upload the artifact and then set the state depending on whether or not
			// it passed.
			err := uploader.Upload(artifact)
			if err != nil {
				artifact.State = "error"
				logger.Error("Error uploading artifact \"%s\": %s", artifact.Path, err)

				// Track the error that was raised
				p.Lock()
				errors = append(errors, err)
				p.Unlock()
			} else {
				artifact.State = "finished"
			}

			// Update the state of the artifact on Buildkite
			err = artifact.Update()
			if err != nil {
				logger.Error("Error marking artifact %s as uploaded: %s", artifact.Path, err)

				// Track the error that was raised
				p.Lock()
				errors = append(errors, err)
				p.Unlock()
			}
		})
	}

	p.Wait()

	if len(errors) > 0 {
		logger.Fatal("There were errors with uploading some of the artifacts")
	}

	return nil
}