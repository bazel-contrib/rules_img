package main

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// errNotInArchive reports that an archive does not contain a file, as opposed to
// the archive being unreadable.
var errNotInArchive = errors.New("not in the archive")

// readFromArchive returns the contents of one file from a gzipped tar.
//
// Entry names are compared after stripping a leading "./", which is how the
// archives produced by rules_pkg spell them.
func readFromArchive(archivePath, name string) ([]byte, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("opening archive: %w", err)
	}
	defer file.Close()

	decompressed, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("decompressing archive %s: %w", archivePath, err)
	}
	defer decompressed.Close()

	reader := tar.NewReader(decompressed)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading archive %s: %w", archivePath, err)
		}
		if archiveEntryName(header.Name) != name {
			continue
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("reading %s from archive: %w", name, err)
		}
		return content, nil
	}
	return nil, fmt.Errorf("archive %s contains no %s: %w", archivePath, name, errNotInArchive)
}

func archiveEntryName(name string) string {
	return strings.TrimPrefix(path.Clean(name), "./")
}
