package ocilayout

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// LayerMetadata is the subset of a layer metadata JSON file the layout writers
// need. It matches the fields the docker-save/oci-layout/sparse-oci-layout
// commands read from their --layer metadata files.
type LayerMetadata struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// HexDigest returns the digest with any "sha256:" prefix removed.
func (m LayerMetadata) HexDigest() string {
	return strings.TrimPrefix(m.Digest, "sha256:")
}

// ReadLayerMetadata reads and parses a layer metadata JSON file.
func ReadLayerMetadata(path string) (LayerMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LayerMetadata{}, fmt.Errorf("reading layer metadata %s: %w", path, err)
	}
	var meta LayerMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return LayerMetadata{}, fmt.Errorf("unmarshaling layer metadata %s: %w", path, err)
	}
	return meta, nil
}
