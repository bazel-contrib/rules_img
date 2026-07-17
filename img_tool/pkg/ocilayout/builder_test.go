package ocilayout

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func makeTestManifest() (*v1.Manifest, []byte, *MemBlobSource, string, string) {
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDigest := hashBytes(configData)

	layerData := []byte("fake layer content")
	layerDigest := hashBytes(layerData)

	manifest := &v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: configDigest, Size: int64(len(configData))},
		Layers:        []v1.Descriptor{{MediaType: types.OCILayer, Digest: layerDigest, Size: int64(len(layerData))}},
	}
	manifestData, _ := json.Marshal(manifest)

	source := NewMemBlobSource().Add(configDigest.Hex, configData).Add(layerDigest.Hex, layerData)
	return manifest, manifestData, source, configDigest.Hex, layerDigest.Hex
}

func manifestInputFromSource(m *v1.Manifest, data []byte, src BlobSource) ManifestInput {
	mi := ManifestInput{Manifest: m, ManifestData: data, Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}
	mi.Config = BlobFromSource(src, m.Config.Digest.Hex, m.Config.Size)
	for _, l := range m.Layers {
		mi.Layers = append(mi.Layers, LayerInput{
			Descriptor: l,
			Blob:       BlobFromSource(src, l.Digest.Hex, l.Size),
			Present:    true,
		})
	}
	return mi
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

func TestDockerSaveSingleManifestTar(t *testing.T) {
	manifest, manifestData, source, configHex, layerHex := makeTestManifest()

	var buf bytes.Buffer
	err := New(DockerSave()).
		WithTags([]string{"registry.io/repo:v1.0"}).
		WithOCITags([]string{"registry.io/repo:v1.0"}).
		AddManifest(manifestInputFromSource(manifest, manifestData, source)).
		WriteToWriter(context.Background(), &buf)
	if err != nil {
		t.Fatalf("WriteToWriter: %v", err)
	}

	files := extractTar(t, &buf)
	for _, want := range []string{"oci-layout", "index.json", "manifest.json", "blobs/", "blobs/sha256/"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}

	var index v1.IndexManifest
	if err := json.Unmarshal(files["index.json"], &index); err != nil {
		t.Fatalf("parsing index.json: %v", err)
	}
	if len(index.Manifests) != 1 {
		t.Fatalf("expected 1 manifest, got %d", len(index.Manifests))
	}
	if index.Manifests[0].Annotations["io.containerd.image.name"] != "registry.io/repo:v1.0" {
		t.Errorf("wrong containerd annotation: %v", index.Manifests[0].Annotations)
	}
	if index.Manifests[0].Annotations["org.opencontainers.image.ref.name"] != "registry.io/repo:v1.0" {
		t.Errorf("wrong ref.name annotation: %v", index.Manifests[0].Annotations)
	}

	var dockerMfsts []dockerManifest
	if err := json.Unmarshal(files["manifest.json"], &dockerMfsts); err != nil {
		t.Fatalf("parsing manifest.json: %v", err)
	}
	if len(dockerMfsts) != 1 || len(dockerMfsts[0].RepoTags) != 1 || dockerMfsts[0].RepoTags[0] != "registry.io/repo:v1.0" {
		t.Errorf("wrong docker manifest: %+v", dockerMfsts)
	}

	manifestHex := hashBytes(manifestData).Hex
	for _, hex := range []string{manifestHex, configHex, layerHex} {
		if _, ok := files["blobs/sha256/"+hex]; !ok {
			t.Errorf("missing blob %s", hex)
		}
	}
}

func TestDockerSaveTagOnly(t *testing.T) {
	manifest, manifestData, source, _, _ := makeTestManifest()

	var buf bytes.Buffer
	err := New(DockerSave().WithAnnotations(AnnotateTagOnly)).
		WithOCITags([]string{"registry.io/repo:v1.0"}).
		AddManifest(manifestInputFromSource(manifest, manifestData, source)).
		WriteToWriter(context.Background(), &buf)
	if err != nil {
		t.Fatalf("WriteToWriter: %v", err)
	}
	files := extractTar(t, &buf)
	var index v1.IndexManifest
	json.Unmarshal(files["index.json"], &index)
	if got := index.Manifests[0].Annotations["org.opencontainers.image.ref.name"]; got != "v1.0" {
		t.Errorf("tagOnly ref.name = %q, want v1.0", got)
	}
}

func TestDockerSaveIndexWrapping(t *testing.T) {
	manifest, manifestData, source, _, _ := makeTestManifest()
	manifestDigest := hashBytes(manifestData)

	indexContent := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeOCIImageIndex,
		Manifests: []v1.Descriptor{{
			MediaType: types.OCIManifestSchema1,
			Digest:    manifestDigest,
			Size:      int64(len(manifestData)),
			Platform:  &v1.Platform{OS: "linux", Architecture: "amd64"},
		}},
	}
	indexData, _ := json.Marshal(indexContent)

	var buf bytes.Buffer
	err := New(DockerSave().WithIndexStyle(IndexWrapping)).
		WithTags([]string{"registry.io/repo:latest"}).
		WithOCITags([]string{"registry.io/repo:latest"}).
		SetRootIndex(BlobFromBytes(indexData)).
		AddManifest(manifestInputFromSource(manifest, manifestData, source)).
		WriteToWriter(context.Background(), &buf)
	if err != nil {
		t.Fatalf("WriteToWriter: %v", err)
	}

	files := extractTar(t, &buf)
	indexDigest := hashBytes(indexData)
	if _, ok := files["blobs/sha256/"+indexDigest.Hex]; !ok {
		t.Error("missing nested index blob")
	}
	var rootIndex v1.IndexManifest
	json.Unmarshal(files["index.json"], &rootIndex)
	if len(rootIndex.Manifests) != 1 || string(rootIndex.Manifests[0].MediaType) != mediaTypeOCIImageIndex {
		t.Errorf("root index should reference the nested index: %+v", rootIndex.Manifests)
	}
}

func TestWrappingWithFilter(t *testing.T) {
	cfg1 := []byte(`{"architecture":"amd64","os":"linux"}`)
	l1 := []byte("amd64 layer")
	cfg2 := []byte(`{"architecture":"arm64","os":"linux"}`)
	l2 := []byte("arm64 layer")
	src := NewMemBlobSource().
		Add(hashBytes(cfg1).Hex, cfg1).Add(hashBytes(l1).Hex, l1).
		Add(hashBytes(cfg2).Hex, cfg2).Add(hashBytes(l2).Hex, l2)

	mk := func(cfg, layer []byte, arch string) (*v1.Manifest, []byte) {
		m := &v1.Manifest{
			SchemaVersion: 2, MediaType: types.OCIManifestSchema1,
			Config: v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: hashBytes(cfg), Size: int64(len(cfg))},
			Layers: []v1.Descriptor{{MediaType: types.OCILayer, Digest: hashBytes(layer), Size: int64(len(layer))}},
		}
		d, _ := json.Marshal(m)
		return m, d
	}
	m1, m1d := mk(cfg1, l1, "amd64")
	m2, m2d := mk(cfg2, l2, "arm64")

	indexContent := v1.IndexManifest{
		SchemaVersion: 2, MediaType: mediaTypeOCIImageIndex,
		Manifests: []v1.Descriptor{
			{MediaType: types.OCIManifestSchema1, Digest: hashBytes(m1d), Size: int64(len(m1d)), Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}},
			{MediaType: types.OCIManifestSchema1, Digest: hashBytes(m2d), Size: int64(len(m2d)), Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}},
		},
	}
	indexData, _ := json.Marshal(indexContent)

	filter := func(manifests []ManifestDescriptor) ([]int, int) {
		for i, m := range manifests {
			if m.Platform != nil && m.Platform.Architecture == "arm64" {
				return []int{i}, i
			}
		}
		return []int{0}, 0
	}

	i1 := ManifestInput{Manifest: m1, ManifestData: m1d, Config: BlobFromSource(src, m1.Config.Digest.Hex, m1.Config.Size)}
	i1.Layers = []LayerInput{{Descriptor: m1.Layers[0], Blob: BlobFromSource(src, m1.Layers[0].Digest.Hex, m1.Layers[0].Size), Present: true}}
	i2 := ManifestInput{Manifest: m2, ManifestData: m2d, Config: BlobFromSource(src, m2.Config.Digest.Hex, m2.Config.Size)}
	i2.Layers = []LayerInput{{Descriptor: m2.Layers[0], Blob: BlobFromSource(src, m2.Layers[0].Digest.Hex, m2.Layers[0].Size), Present: true}}

	var buf bytes.Buffer
	err := New(DockerSave().WithIndexStyle(IndexWrapping)).
		WithTags([]string{"repo:tag"}).WithOCITags([]string{"repo:tag"}).
		WithManifestFilter(filter).
		SetRootIndex(BlobFromBytes(indexData)).
		AddManifest(i1).AddManifest(i2).
		WriteToWriter(context.Background(), &buf)
	if err != nil {
		t.Fatalf("WriteToWriter: %v", err)
	}

	files := extractTar(t, &buf)
	if _, ok := files["blobs/sha256/"+hashBytes(cfg2).Hex]; !ok {
		t.Error("missing arm64 config blob")
	}
	if _, ok := files["blobs/sha256/"+hashBytes(cfg1).Hex]; ok {
		t.Error("amd64 config blob should not be included")
	}
	var dockerMfsts []dockerManifest
	json.Unmarshal(files["manifest.json"], &dockerMfsts)
	if dockerMfsts[0].Config != "blobs/sha256/"+hashBytes(cfg2).Hex {
		t.Errorf("docker manifest should reference arm64 config, got %s", dockerMfsts[0].Config)
	}
}

func TestOCILayoutDirectoryCleanIndex(t *testing.T) {
	manifest, manifestData, _, configHex, layerHex := makeTestManifest()
	dir := t.TempDir()

	// Write config + layer to files so the directory sink can copy them.
	configPath := filepath.Join(dir, "config")
	os.WriteFile(configPath, []byte(`{"architecture":"amd64","os":"linux"}`), 0o644)
	layerPath := filepath.Join(dir, "layer")
	os.WriteFile(layerPath, []byte("fake layer content"), 0o644)

	out := filepath.Join(dir, "layout")
	mi := ManifestInput{Manifest: manifest, ManifestData: manifestData, Config: BlobFromPath(configPath)}
	mi.Layers = []LayerInput{{Descriptor: manifest.Layers[0], Blob: BlobFromPath(layerPath), Present: true}}

	if err := New(OCILayout()).AddManifest(mi).WriteDir(context.Background(), out); err != nil {
		t.Fatalf("WriteDir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(out, "manifest.json")); !os.IsNotExist(err) {
		t.Error("oci-layout must not contain a Docker manifest.json")
	}
	idxData, err := os.ReadFile(filepath.Join(out, "index.json"))
	if err != nil {
		t.Fatalf("reading index.json: %v", err)
	}
	var idx v1.IndexManifest
	json.Unmarshal(idxData, &idx)
	if len(idx.Manifests) != 1 {
		t.Fatalf("clean index should have exactly 1 descriptor, got %d", len(idx.Manifests))
	}
	if idx.Manifests[0].Annotations != nil {
		t.Errorf("clean index descriptor should have no annotations, got %v", idx.Manifests[0].Annotations)
	}
	manifestHex := hashBytes(manifestData).Hex
	for _, hex := range []string{manifestHex, configHex, layerHex} {
		if _, err := os.Stat(filepath.Join(out, "blobs/sha256", hex)); err != nil {
			t.Errorf("missing blob %s: %v", hex, err)
		}
	}
}

func TestSparseLayout(t *testing.T) {
	manifest, manifestData, _, configHex, layerHex := makeTestManifest()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	os.WriteFile(configPath, []byte(`{"architecture":"amd64","os":"linux"}`), 0o644)
	out := filepath.Join(dir, "sparse")

	mi := ManifestInput{Manifest: manifest, ManifestData: manifestData, Config: BlobFromPath(configPath)}
	mi.Layers = []LayerInput{{Descriptor: manifest.Layers[0]}} // no body, sparse

	if err := New(SparseOCILayout()).AddManifest(mi).WriteDir(context.Background(), out); err != nil {
		t.Fatalf("WriteDir: %v", err)
	}

	if _, err := os.Stat(filepath.Join(out, "sparse-oci-layout")); err != nil {
		t.Errorf("missing sparse marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "root.descriptor.json")); err != nil {
		t.Errorf("missing root.descriptor.json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "index.json")); !os.IsNotExist(err) {
		t.Error("sparse layout must not have index.json")
	}
	if _, err := os.Stat(filepath.Join(out, "blobs/sha256", configHex)); err != nil {
		t.Errorf("config blob should be present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "blobs/sha256", layerHex)); !os.IsNotExist(err) {
		t.Error("layer body must NOT be present in sparse layout")
	}
	if _, err := os.Stat(filepath.Join(out, "blobs/sha256", layerHex+".descriptor.json")); err != nil {
		t.Errorf("layer descriptor should be present: %v", err)
	}
}

func TestMissingBlobs(t *testing.T) {
	manifest, manifestData, _, _, _ := makeTestManifest()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	os.WriteFile(configPath, []byte(`{"architecture":"amd64","os":"linux"}`), 0o644)

	mi := ManifestInput{Manifest: manifest, ManifestData: manifestData, Config: BlobFromPath(configPath)}
	mi.Layers = []LayerInput{{Descriptor: manifest.Layers[0], Present: false}} // missing body

	err := New(OCILayout()).WithMissingBlobsHint(OutputGroupOCILayout).
		AddManifest(mi).WriteDir(context.Background(), filepath.Join(dir, "out"))
	var mbe *MissingBlobsError
	if !errors.As(err, &mbe) {
		t.Fatalf("expected MissingBlobsError, got %v", err)
	}
	if len(mbe.MissingBlobs) != 1 {
		t.Errorf("expected 1 missing blob, got %v", mbe.MissingBlobs)
	}
}
