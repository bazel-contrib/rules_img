package ocitar

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type memBlobSource struct {
	blobs map[string][]byte
}

func (m *memBlobSource) OpenBlob(_ context.Context, hexDigest string) (io.ReadCloser, int64, error) {
	data, ok := m.blobs[hexDigest]
	if !ok {
		return nil, 0, io.ErrUnexpectedEOF
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func makeTestManifest() (*v1.Manifest, []byte, *memBlobSource) {
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := hashBytes(configData)

	layerData := []byte("fake layer content")
	layerDigest := hashBytes(layerData)

	manifest := &v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config: v1.Descriptor{
			MediaType: types.OCIConfigJSON,
			Digest:    configDigest,
			Size:      int64(len(configData)),
		},
		Layers: []v1.Descriptor{
			{
				MediaType: types.OCILayer,
				Digest:    layerDigest,
				Size:      int64(len(layerData)),
			},
		},
	}

	manifestData, _ := json.Marshal(manifest)

	source := &memBlobSource{
		blobs: map[string][]byte{
			configDigest.Hex: configData,
			layerDigest.Hex:  layerData,
		},
	}

	return manifest, manifestData, source
}

func TestWriteSingleManifest(t *testing.T) {
	manifest, manifestData, source := makeTestManifest()

	var buf bytes.Buffer
	opts := Options{
		Tags:    []string{"registry.io/repo:v1.0"},
		OCITags: []string{"registry.io/repo:v1.0"},
	}

	err := WriteSingleManifest(context.Background(), &buf, manifest, manifestData, source, opts)
	if err != nil {
		t.Fatalf("WriteSingleManifest failed: %v", err)
	}

	// Parse the tar and verify structure
	files := extractTar(t, &buf)

	// Verify oci-layout exists
	if _, ok := files["oci-layout"]; !ok {
		t.Error("missing oci-layout")
	}

	// Verify index.json
	indexData, ok := files["index.json"]
	if !ok {
		t.Fatal("missing index.json")
	}
	var index v1.IndexManifest
	if err := json.Unmarshal(indexData, &index); err != nil {
		t.Fatalf("parsing index.json: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("expected 1 manifest in index, got %d", len(index.Manifests))
	}
	if index.Manifests[0].Annotations["io.containerd.image.name"] != "registry.io/repo:v1.0" {
		t.Errorf("wrong containerd annotation: %v", index.Manifests[0].Annotations)
	}
	if index.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != "v1.0" {
		t.Errorf("wrong ref.name annotation: %v", index.Manifests[0].Annotations)
	}

	// Verify manifest.json (Docker format)
	mfstData, ok := files["manifest.json"]
	if !ok {
		t.Fatal("missing manifest.json")
	}
	var dockerMfsts []dockerManifest
	if err := json.Unmarshal(mfstData, &dockerMfsts); err != nil {
		t.Fatalf("parsing manifest.json: %v", err)
	}
	if len(dockerMfsts) != 1 {
		t.Fatalf("expected 1 docker manifest, got %d", len(dockerMfsts))
	}
	if len(dockerMfsts[0].RepoTags) != 1 || dockerMfsts[0].RepoTags[0] != "registry.io/repo:v1.0" {
		t.Errorf("wrong RepoTags: %v", dockerMfsts[0].RepoTags)
	}

	// Verify blobs exist
	manifestDigest := hashBytes(manifestData)
	if _, ok := files["blobs/sha256/"+manifestDigest.Hex]; !ok {
		t.Error("missing manifest blob")
	}
	if _, ok := files["blobs/sha256/"+manifest.Config.Digest.Hex]; !ok {
		t.Error("missing config blob")
	}
	if _, ok := files["blobs/sha256/"+manifest.Layers[0].Digest.Hex]; !ok {
		t.Error("missing layer blob")
	}
}

func TestWriteIndex(t *testing.T) {
	manifest, manifestData, source := makeTestManifest()
	manifestDigest := hashBytes(manifestData)

	// Build an index referencing this manifest
	indexContent := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests: []v1.Descriptor{
			{
				MediaType: types.OCIManifestSchema1,
				Digest:    manifestDigest,
				Size:      int64(len(manifestData)),
				Platform: &v1.Platform{
					OS:           "linux",
					Architecture: "amd64",
				},
			},
		},
	}
	indexData, _ := json.Marshal(indexContent)

	manifestInfos := []ManifestInfo{
		{
			ManifestData: manifestData,
			ConfigDigest: manifest.Config.Digest.Hex,
			LayerDigests: []string{manifest.Layers[0].Digest.Hex},
			MediaType:    types.OCIManifestSchema1,
		},
	}

	var buf bytes.Buffer
	opts := Options{
		Tags:    []string{"registry.io/repo:latest"},
		OCITags: []string{"registry.io/repo:latest"},
	}

	err := WriteIndex(context.Background(), &buf, indexData, manifestInfos, source, opts)
	if err != nil {
		t.Fatalf("WriteIndex failed: %v", err)
	}

	files := extractTar(t, &buf)

	// Verify index.json wraps the original index
	rootIndexData, ok := files["index.json"]
	if !ok {
		t.Fatal("missing index.json")
	}
	var rootIndex v1.IndexManifest
	if err := json.Unmarshal(rootIndexData, &rootIndex); err != nil {
		t.Fatalf("parsing root index.json: %v", err)
	}
	if len(rootIndex.Manifests) != 1 {
		t.Fatalf("expected 1 entry in root index, got %d", len(rootIndex.Manifests))
	}
	// Root index should reference the original index blob (mediaType = index)
	if string(rootIndex.Manifests[0].MediaType) != "application/vnd.oci.image.index.v1+json" {
		t.Errorf("wrong mediaType in root index: %s", rootIndex.Manifests[0].MediaType)
	}

	// Verify the index blob is stored
	indexDigest := hashBytes(indexData)
	if _, ok := files["blobs/sha256/"+indexDigest.Hex]; !ok {
		t.Error("missing index blob in blobs/")
	}

	// Verify manifest blob
	if _, ok := files["blobs/sha256/"+manifestDigest.Hex]; !ok {
		t.Error("missing manifest blob")
	}

	// Verify config and layer blobs
	if _, ok := files["blobs/sha256/"+manifest.Config.Digest.Hex]; !ok {
		t.Error("missing config blob")
	}
	if _, ok := files["blobs/sha256/"+manifest.Layers[0].Digest.Hex]; !ok {
		t.Error("missing layer blob")
	}
}

func TestWriteIndexWithFilter(t *testing.T) {
	// Create two manifests for different platforms
	configData1 := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest1 := hashBytes(configData1)
	layerData1 := []byte("amd64 layer")
	layerDigest1 := hashBytes(layerData1)

	configData2 := []byte(`{"architecture":"arm64","os":"linux"}`)
	configDigest2 := hashBytes(configData2)
	layerData2 := []byte("arm64 layer")
	layerDigest2 := hashBytes(layerData2)

	manifest1 := &v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: configDigest1, Size: int64(len(configData1))},
		Layers:        []v1.Descriptor{{MediaType: types.OCILayer, Digest: layerDigest1, Size: int64(len(layerData1))}},
	}
	manifest1Data, _ := json.Marshal(manifest1)
	manifest1Digest := hashBytes(manifest1Data)

	manifest2 := &v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: configDigest2, Size: int64(len(configData2))},
		Layers:        []v1.Descriptor{{MediaType: types.OCILayer, Digest: layerDigest2, Size: int64(len(layerData2))}},
	}
	manifest2Data, _ := json.Marshal(manifest2)
	manifest2Digest := hashBytes(manifest2Data)

	indexContent := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.oci.image.index.v1+json",
		Manifests: []v1.Descriptor{
			{MediaType: types.OCIManifestSchema1, Digest: manifest1Digest, Size: int64(len(manifest1Data)), Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
			{MediaType: types.OCIManifestSchema1, Digest: manifest2Digest, Size: int64(len(manifest2Data)), Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
		},
	}
	indexData, _ := json.Marshal(indexContent)

	source := &memBlobSource{
		blobs: map[string][]byte{
			configDigest1.Hex: configData1,
			layerDigest1.Hex:  layerData1,
			configDigest2.Hex: configData2,
			layerDigest2.Hex:  layerData2,
		},
	}

	manifestInfos := []ManifestInfo{
		{ManifestData: manifest1Data, ConfigDigest: configDigest1.Hex, LayerDigests: []string{layerDigest1.Hex}, MediaType: types.OCIManifestSchema1},
		{ManifestData: manifest2Data, ConfigDigest: configDigest2.Hex, LayerDigests: []string{layerDigest2.Hex}, MediaType: types.OCIManifestSchema1},
	}

	// Filter: only include arm64, make it the default
	filter := func(manifests []ManifestDescriptor) ([]int, int) {
		for i, m := range manifests {
			if m.Platform != nil && m.Platform.Architecture == "arm64" {
				return []int{i}, i
			}
		}
		return []int{0}, 0
	}

	var buf bytes.Buffer
	opts := Options{
		Tags:           []string{"repo:tag"},
		OCITags:        []string{"repo:tag"},
		ManifestFilter: filter,
	}

	err := WriteIndex(context.Background(), &buf, indexData, manifestInfos, source, opts)
	if err != nil {
		t.Fatalf("WriteIndex with filter failed: %v", err)
	}

	files := extractTar(t, &buf)

	// arm64 blobs should be present
	if _, ok := files["blobs/sha256/"+configDigest2.Hex]; !ok {
		t.Error("missing arm64 config blob")
	}
	if _, ok := files["blobs/sha256/"+layerDigest2.Hex]; !ok {
		t.Error("missing arm64 layer blob")
	}

	// amd64 blobs should NOT be present
	if _, ok := files["blobs/sha256/"+configDigest1.Hex]; ok {
		t.Error("amd64 config blob should not be included")
	}
	if _, ok := files["blobs/sha256/"+layerDigest1.Hex]; ok {
		t.Error("amd64 layer blob should not be included")
	}

	// Docker manifest.json should reference arm64
	var dockerMfsts []dockerManifest
	json.Unmarshal(files["manifest.json"], &dockerMfsts)
	if dockerMfsts[0].Config != "blobs/sha256/"+configDigest2.Hex {
		t.Errorf("docker manifest should reference arm64 config, got %s", dockerMfsts[0].Config)
	}
}

func extractTar(t *testing.T, r io.Reader) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeDir {
			files[hdr.Name] = nil
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading tar entry %s: %v", hdr.Name, err)
		}
		files[hdr.Name] = data
	}
	return files
}
