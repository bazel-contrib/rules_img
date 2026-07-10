package dockersave

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestDockerSaveReconstructsCompactStreamLayer(t *testing.T) {
	// Isolate each resolver path from cache settings inherited by the test process.
	t.Setenv("IMG_DISK_CACHE", "")
	t.Setenv("IMG_REAPI_ENDPOINT", "")
	t.Setenv("IMG_REAPI_INSTANCE_NAME", "")
	t.Setenv("IMG_CREDENTIAL_HELPER", "")

	// Build a compact stream whose final layer mixes inline and CAS-backed bytes.
	dir := t.TempDir()
	layerBytes := []byte("layer-prefix-CAS-CONTENT-layer-suffix")
	cstreamPath, casDir := writeTestCompactStream(t, dir, layerBytes)

	layerDigest := sha256.Sum256(layerBytes)
	layerHex := hex.EncodeToString(layerDigest[:])
	configBytes := []byte(`{"architecture":"amd64","os":"linux","rootfs":{"type":"layers","diff_ids":[]}}`)
	configDigest := sha256.Sum256(configBytes)
	configHex := hex.EncodeToString(configDigest[:])

	configPath := filepath.Join(dir, "config.json")
	writeFile(t, configPath, configBytes)

	manifest := v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config: v1.Descriptor{
			MediaType: types.OCIConfigJSON,
			Digest:    v1.Hash{Algorithm: "sha256", Hex: configHex},
			Size:      int64(len(configBytes)),
		},
		Layers: []v1.Descriptor{{
			MediaType: types.OCILayer,
			Digest:    v1.Hash{Algorithm: "sha256", Hex: layerHex},
			Size:      int64(len(layerBytes)),
		}},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	writeFile(t, manifestPath, manifestData)

	metadataPath := filepath.Join(dir, "layer_metadata.json")
	writeJSON(t, metadataPath, map[string]any{
		"digest":    "sha256:" + layerHex,
		"mediaType": string(types.OCILayer),
		"size":      len(layerBytes),
	})

	layers := layerMappingFlag{{metadata: metadataPath, blob: cstreamPath}}
	layerCASDirs := layerMappingFlag{{metadata: metadataPath, blob: casDir}}

	// Verify a single-image tar contains the reconstructed layer rather than the recipe.
	tarPath := filepath.Join(dir, "image.tar")
	if err := assembleDockerSave(context.Background(), manifestPath, configPath, tarPath, "tar", layers, layerCASDirs, []string{"example:latest"}, []string{"example:latest"}, false, false); err != nil {
		t.Fatal(err)
	}
	if got := readTarEntry(t, tarPath, filepath.ToSlash(filepath.Join("blobs", "sha256", layerHex))); !bytes.Equal(got, layerBytes) {
		t.Fatalf("tar layer bytes mismatch:\ngot  %q\nwant %q", got, layerBytes)
	}

	// Verify directory output streams the same reconstructed bytes to the digest path.
	outDir := filepath.Join(dir, "docker-save")
	if err := assembleDockerSave(context.Background(), manifestPath, configPath, outDir, "directory", layers, layerCASDirs, []string{"example:latest"}, []string{"example:latest"}, true, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(outDir, "blobs", "sha256", layerHex))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, layerBytes) {
		t.Fatalf("directory layer bytes mismatch:\ngot  %q\nwant %q", got, layerBytes)
	}

	// Verify the multi-platform index path uses the same compact-layer source.
	manifestDigest := sha256.Sum256(manifestData)
	index := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		Manifests: []v1.Descriptor{{
			MediaType: manifest.MediaType,
			Digest:    v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(manifestDigest[:])},
			Size:      int64(len(manifestData)),
			Platform:  &v1.Platform{Architecture: "amd64", OS: "linux"},
		}},
	}
	indexData, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(dir, "index.json")
	writeFile(t, indexPath, indexData)

	indexTarPath := filepath.Join(dir, "image-index.tar")
	if err := assembleDockerSaveWithIndex(context.Background(), indexPath, indexTarPath, "tar", []string{manifestPath}, []string{configPath}, layers, layerCASDirs, []string{"example:latest"}, []string{"example:latest"}, false, false); err != nil {
		t.Fatal(err)
	}
	if got := readTarEntry(t, indexTarPath, filepath.ToSlash(filepath.Join("blobs", "sha256", layerHex))); !bytes.Equal(got, layerBytes) {
		t.Fatalf("index tar layer bytes mismatch:\ngot  %q\nwant %q", got, layerBytes)
	}

	// Verify reconstruction succeeds with no local CAS directory when disk cache has the blob.
	casBlob := []byte("CAS-CONTENT")
	casDigest := sha256.Sum256(casBlob)
	casHex := hex.EncodeToString(casDigest[:])
	diskCachePath := filepath.Join(dir, "disk-cache")
	if err := os.MkdirAll(filepath.Join(diskCachePath, "cas", casHex[:2]), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(diskCachePath, "cas", casHex[:2], casHex), casBlob)
	t.Setenv("IMG_DISK_CACHE", diskCachePath)

	diskCacheTarPath := filepath.Join(dir, "disk-cache.tar")
	if err := assembleDockerSave(context.Background(), manifestPath, configPath, diskCacheTarPath, "tar", layers, nil, []string{"example:latest"}, []string{"example:latest"}, false, false); err != nil {
		t.Fatal(err)
	}
	if got := readTarEntry(t, diskCacheTarPath, filepath.ToSlash(filepath.Join("blobs", "sha256", layerHex))); !bytes.Equal(got, layerBytes) {
		t.Fatalf("disk-cache tar layer bytes mismatch:\ngot  %q\nwant %q", got, layerBytes)
	}

	// Verify failure explains every resolver tier when no referenced content is available.
	t.Setenv("IMG_DISK_CACHE", "")

	missingCASTarPath := filepath.Join(dir, "missing-cas.tar")
	err = assembleDockerSave(context.Background(), manifestPath, configPath, missingCASTarPath, "tar", layers, nil, []string{"example:latest"}, []string{"example:latest"}, false, false)
	if err == nil {
		t.Fatal("expected compact stream reconstruction to fail without a CAS directory")
	}
	if !strings.Contains(err.Error(), "not found in input file CAS directory, disk cache, or remote cache") {
		t.Fatalf("missing compact-stream content error = %q, want it to mention all CAS lookup sources", err)
	}
}

// writeTestCompactStream creates a recipe with inline prefix/suffix and one CAS reference.
func writeTestCompactStream(t *testing.T, dir string, layerBytes []byte) (string, string) {
	t.Helper()

	prefix := []byte("layer-prefix-")
	casBlob := []byte("CAS-CONTENT")
	suffix := []byte("-layer-suffix")
	if !bytes.Equal(layerBytes, append(append(append([]byte(nil), prefix...), casBlob...), suffix...)) {
		t.Fatal("test layer bytes do not match compact stream parts")
	}

	casDigest := sha256.Sum256(casBlob)
	layerDigest := sha256.Sum256(layerBytes)

	casDir := filepath.Join(dir, "cas")
	casShaDir := filepath.Join(casDir, "sha256")
	if err := os.MkdirAll(casShaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(casShaDir, hex.EncodeToString(casDigest[:])), casBlob)

	cstreamPath := filepath.Join(dir, "layer.cstream")
	f, err := os.Create(cstreamPath)
	if err != nil {
		t.Fatal(err)
	}
	writer := compactstream.NewWriter(f, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionNone, compactstream.OriginalCompressionInfo{}, 0)
	if err := writer.SetCompressedStreamInfo(layerDigest[:], uint64(len(layerBytes))); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteStreamBytes(prefix); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteCASRef(casDigest[:], uint64(len(casBlob))); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteStreamBytes(suffix); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return cstreamPath, casDir
}

// writeFile writes deterministic test data and fails the current test on error.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeJSON serializes a test fixture to disk.
func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, data)
}

// readTarEntry returns one named entry from a generated Docker-save tarball.
func readTarEntry(t *testing.T, tarPath, entryPath string) []byte {
	t.Helper()
	f, err := os.Open(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name != entryPath {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	t.Fatalf("tar entry %s not found", entryPath)
	return nil
}
