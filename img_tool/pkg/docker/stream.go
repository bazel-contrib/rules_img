package docker

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"path"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/progress"
	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TarWriter streams a docker-compatible tar file
type TarWriter struct {
	tw           *tar.Writer
	manifestData []ManifestEntry
}

// ManifestEntry represents an entry in the manifest.json
type ManifestEntry struct {
	Config       string   `json:"Config"`
	RepoTags     []string `json:"RepoTags"`
	Layers       []string `json:"Layers"`
	Architecture string   `json:"Architecture,omitempty"`
	Os           string   `json:"Os,omitempty"`
}

// NewTarWriter creates a new streaming tar writer
func NewTarWriter(w io.Writer) *TarWriter {
	return &TarWriter{
		tw: tar.NewWriter(w),
	}
}

// WriteConfig writes the config JSON to the tar
func (t *TarWriter) WriteConfig(configData []byte) error {
	configName := fmt.Sprintf("%s.json", digest.FromBytes(configData).Hex())

	// Parse config to extract architecture and OS
	var imageConfig ocispec.Image
	if err := json.Unmarshal(configData, &imageConfig); err != nil {
		return fmt.Errorf("parsing config: %w", err)
	}

	// Store manifest entry
	t.manifestData = append(t.manifestData, ManifestEntry{
		Config:       configName,
		Architecture: imageConfig.Architecture,
		Os:           imageConfig.OS,
		Layers:       []string{}, // Will be populated as layers are written
	})

	// Write config file
	return t.writeFile(configName, configData)
}

// WriteLayer streams a layer to the tar
func (t *TarWriter) WriteLayer(layerDigest registryv1.Hash, size int64, reader io.Reader) error {
	layerDir := layerDigest.Hex
	layerPath := path.Join(layerDir, "layer.tar")

	// Update manifest entry
	if len(t.manifestData) > 0 {
		t.manifestData[len(t.manifestData)-1].Layers = append(
			t.manifestData[len(t.manifestData)-1].Layers,
			layerPath,
		)
	}

	// Create directory entry
	if err := t.tw.WriteHeader(&tar.Header{
		Name:     layerDir + "/",
		Mode:     0755,
		Typeflag: tar.TypeDir,
	}); err != nil {
		return fmt.Errorf("writing layer directory: %w", err)
	}

	// Write VERSION file
	if err := t.writeFile(path.Join(layerDir, "VERSION"), []byte("1.0")); err != nil {
		return fmt.Errorf("writing VERSION: %w", err)
	}

	// Write layer content
	hdr := &tar.Header{
		Name: layerPath,
		Mode: 0644,
		Size: size,
	}
	if err := t.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("writing layer header: %w", err)
	}

	// Stream the layer content
	pw := progress.Writer(size, layerDigest.Hex[:12])
	n, err := io.Copy(io.MultiWriter(t.tw, pw), reader)
	if err != nil {
		return fmt.Errorf("streaming layer content: %w", err)
	}
	if n != size {
		return fmt.Errorf("layer size mismatch: expected %d, got %d", size, n)
	}

	return nil
}

// SetTags sets the repository tags for the image
func (t *TarWriter) SetTags(tags []string) {
	if len(t.manifestData) > 0 {
		t.manifestData[len(t.manifestData)-1].RepoTags = tags
	}
}

// Finalize writes the manifest.json and closes the tar
func (t *TarWriter) Finalize() error {
	// Write manifest.json
	manifestJSON, err := json.Marshal(t.manifestData)
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}

	if err := t.writeFile("manifest.json", manifestJSON); err != nil {
		return fmt.Errorf("writing manifest.json: %w", err)
	}

	// Close the tar writer
	return t.tw.Close()
}

// writeFile is a helper to write a file to the tar
func (t *TarWriter) writeFile(name string, data []byte) error {
	hdr := &tar.Header{
		Name: name,
		Mode: 0644,
		Size: int64(len(data)),
	}
	if err := t.tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := t.tw.Write(data)
	return err
}
