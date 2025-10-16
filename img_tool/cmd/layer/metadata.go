package layer

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// FileMetadata represents the metadata that can be set on files in a layer
type FileMetadata struct {
	Mode       *string           `json:"mode,omitempty"`
	Uid        *int              `json:"uid,omitempty"`
	Gid        *int              `json:"gid,omitempty"`
	Uname      *string           `json:"uname,omitempty"`
	Gname      *string           `json:"gname,omitempty"`
	Mtime      *string           `json:"mtime,omitempty"`
	PAXRecords map[string]string `json:"pax_records,omitempty"`
}

// LayerMetadata holds all metadata configuration for a layer
type LayerMetadata struct {
	Defaults      *FileMetadata
	FileOverrides map[string]*FileMetadata
	usageCounts   map[string]int // tracks how many times each FileOverride entry is used
}

// ParseLayerMetadata parses the default metadata and file-specific metadata
func ParseLayerMetadata(defaultJSON string, fileMetadata map[string]string) (*LayerMetadata, error) {
	result := &LayerMetadata{
		FileOverrides: make(map[string]*FileMetadata),
		usageCounts:   make(map[string]int),
	}

	// Parse default metadata if provided
	if defaultJSON != "" {
		var defaults FileMetadata
		if err := json.Unmarshal([]byte(defaultJSON), &defaults); err != nil {
			return nil, fmt.Errorf("invalid default metadata JSON: %w", err)
		}
		result.Defaults = &defaults
	}

	// Parse file-specific metadata
	for path, jsonStr := range fileMetadata {
		var metadata FileMetadata
		if err := json.Unmarshal([]byte(jsonStr), &metadata); err != nil {
			return nil, fmt.Errorf("invalid metadata JSON for path %s: %w", path, err)
		}
		if strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("file path %s should not start with a slash", path)
		}
		result.FileOverrides[path] = &metadata
		result.usageCounts[path] = 0 // initialize usage counter
	}

	return result, nil
}

// ApplyToHeader applies the metadata to a tar header, with file-specific overrides taking precedence
// This implements the tree.MetadataProvider interface
func (lm *LayerMetadata) ApplyToHeader(hdr *tar.Header, pathInImage string) error {
	// First apply defaults
	if lm.Defaults != nil {
		if err := applyFileMetadata(hdr, lm.Defaults); err != nil {
			return fmt.Errorf("applying default metadata: %w", err)
		}
	}

	// Then apply file-specific overrides
	if fileMetadata, ok := lm.FileOverrides[pathInImage]; ok {
		lm.usageCounts[pathInImage]++ // increment usage counter
		if err := applyFileMetadata(hdr, fileMetadata); err != nil {
			return fmt.Errorf("applying metadata for %s: %w", pathInImage, err)
		}
	}

	return nil
}

// VerifyAllFileMetadataUsed checks if all file metadata entries have been used at least once
// Returns an error if any entries are unused, listing all unused paths
func (lm *LayerMetadata) VerifyAllFileMetadataUsed() error {
	if lm == nil || len(lm.FileOverrides) == 0 {
		return nil // no file metadata to verify
	}

	var unusedPaths []string
	for path, count := range lm.usageCounts {
		if count == 0 {
			unusedPaths = append(unusedPaths, path)
		}
	}

	if len(unusedPaths) > 0 {
		slices.Sort(unusedPaths) // sort for consistent error messages
		if len(unusedPaths) == 1 {
			return fmt.Errorf("unused file metadata for path: %s", unusedPaths[0])
		} else {
			return fmt.Errorf("unused file metadata for paths: %s", strings.Join(unusedPaths, ", "))
		}
	}

	return nil
}

// applyFileMetadata applies metadata fields to a tar header
func applyFileMetadata(hdr *tar.Header, metadata *FileMetadata) error {
	if metadata.Mode != nil {
		mode, err := strconv.ParseInt(*metadata.Mode, 8, 64)
		if err != nil {
			return fmt.Errorf("invalid mode %s: %w", *metadata.Mode, err)
		}
		hdr.Mode = mode
	}

	if metadata.Uid != nil {
		hdr.Uid = *metadata.Uid
	}

	if metadata.Gid != nil {
		hdr.Gid = *metadata.Gid
	}

	if metadata.Uname != nil {
		hdr.Uname = *metadata.Uname
	}

	if metadata.Gname != nil {
		hdr.Gname = *metadata.Gname
	}

	if metadata.Mtime != nil {
		// Try parsing as RFC3339 first
		t, err := time.Parse(time.RFC3339, *metadata.Mtime)
		if err != nil {
			// If that fails, try parsing as Unix epoch seconds (int)
			epochSeconds, parseErr := strconv.ParseInt(*metadata.Mtime, 10, 64)
			if parseErr != nil {
				return fmt.Errorf("invalid mtime %s: expected RFC3339 or Unix epoch seconds, got parse errors: RFC3339: %w, epoch: %v", *metadata.Mtime, err, parseErr)
			}
			t = time.Unix(epochSeconds, 0).UTC()
		}
		hdr.ModTime = t
	}

	if metadata.PAXRecords != nil {
		if hdr.PAXRecords == nil {
			hdr.PAXRecords = make(map[string]string)
		}
		for k, v := range metadata.PAXRecords {
			hdr.PAXRecords[k] = v
		}
	}

	return nil
}
