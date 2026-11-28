package deploymetadata

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

const testLayerBLAKE3 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func writeLayerMetadataFile(t *testing.T, dir, name string, descriptor api.Descriptor) string {
	t.Helper()
	data, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func setLayerMetadataFiles(t *testing.T, files map[[2]int]string) {
	t.Helper()
	layerMetadataFiles = newDoubleIndexedStringFlag()
	for index, path := range files {
		if err := layerMetadataFiles.Set(fmt.Sprintf("%d,%d=%s", index[0], index[1], path)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { layerMetadataFiles = nil })
}

func TestCASDigestsForLayer(t *testing.T) {
	dir := t.TempDir()
	withDigest := writeLayerMetadataFile(t, dir, "with.json", api.Descriptor{
		Digest: testLayerDigestA, Size: 100, CASDigests: map[string]string{api.DigestBLAKE3: testLayerBLAKE3},
	})
	without := writeLayerMetadataFile(t, dir, "without.json", api.Descriptor{Digest: testLayerDigestA, Size: 100})
	otherLayer := writeLayerMetadataFile(t, dir, "other.json", api.Descriptor{
		Digest: testLayerDigestB, Size: 100, CASDigests: map[string]string{api.DigestBLAKE3: testLayerBLAKE3},
	})
	otherSize := writeLayerMetadataFile(t, dir, "size.json", api.Descriptor{
		Digest: testLayerDigestA, Size: 7, CASDigests: map[string]string{api.DigestBLAKE3: testLayerBLAKE3},
	})

	t.Run("no flags", func(t *testing.T) {
		layerMetadataFiles = nil
		if got, err := casDigestsForLayer(0, 0, testLayerDigestA, 100); got != nil || err != nil {
			t.Errorf("casDigestsForLayer = %v, %v; want nil, nil", got, err)
		}
	})
	setLayerMetadataFiles(t, map[[2]int]string{{0, 0}: withDigest, {0, 1}: without, {1, 0}: otherLayer, {1, 1}: otherSize})

	t.Run("recorded digest is copied", func(t *testing.T) {
		got, err := casDigestsForLayer(0, 0, testLayerDigestA, 100)
		if err != nil {
			t.Fatal(err)
		}
		if want := map[string]string{api.DigestBLAKE3: testLayerBLAKE3}; !reflect.DeepEqual(got, want) {
			t.Errorf("casDigestsForLayer = %v, want %v", got, want)
		}
	})
	t.Run("metadata without a digest", func(t *testing.T) {
		if got, err := casDigestsForLayer(0, 1, testLayerDigestA, 100); got != nil || err != nil {
			t.Errorf("casDigestsForLayer = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("layer without a metadata file", func(t *testing.T) {
		if got, err := casDigestsForLayer(0, 5, testLayerDigestA, 100); got != nil || err != nil {
			t.Errorf("casDigestsForLayer = %v, %v; want nil, nil", got, err)
		}
	})
	t.Run("metadata of another layer is an error", func(t *testing.T) {
		if _, err := casDigestsForLayer(1, 0, testLayerDigestA, 100); err == nil || !strings.Contains(err.Error(), "describes") {
			t.Errorf("error = %v, want a mismatch error", err)
		}
	})
	t.Run("metadata of another size is an error", func(t *testing.T) {
		if _, err := casDigestsForLayer(1, 1, testLayerDigestA, 100); err == nil {
			t.Error("a layer metadata file of a different size was accepted")
		}
	})
}

// TestWriteMetadataRecordsCASDigests covers the whole path: the BLAKE3 digest of
// a layer, recorded by its build in the layer metadata, ends up on the layer's
// descriptor in the deploy manifest, next to the SHA256 digest that is in the
// image manifest.
func TestWriteMetadataRecordsCASDigests(t *testing.T) {
	tmp := t.TempDir()
	manifestJSON := `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "` + testConfigDigest + `", "size": 42},
  "layers": [
    {"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": "` + testLayerDigestA + `", "size": 100},
    {"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": "` + testLayerDigestB + `", "size": 200}
  ]
}`
	manifestPath := filepath.Join(tmp, "manifest.json")
	configPath := filepath.Join(tmp, "config.json")
	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"registry":"registry.example.com","repository":"team/app","tags":["latest"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	setLayerMetadataFiles(t, map[[2]int]string{
		{0, 0}: writeLayerMetadataFile(t, tmp, "a.json", api.Descriptor{Digest: testLayerDigestA, Size: 100, CASDigests: map[string]string{api.DigestBLAKE3: testLayerBLAKE3}}),
		// A layer that was built without recording a digest.
		{0, 1}: writeLayerMetadataFile(t, tmp, "b.json", api.Descriptor{Digest: testLayerDigestB, Size: 200}),
	})

	command = "push"
	strategy = "lazy"
	rootPath = manifestPath
	rootKind = "manifest"
	configurationPath = configPath
	manifestPaths = []string{manifestPath}
	destinationFilePath = ""
	crossMountStrategy = ""
	crossMountFromManifestPath = ""
	manifestTagFiles = nil
	originalRegistries = nil
	originalRepository = ""
	orginalTag = ""
	originalDigest = ""
	referrerRootPaths = newIndexedStringFlag()
	layerCompactStreams = newDoubleIndexedStringFlag()
	layerSourcesForManifest = nil

	outputPath := filepath.Join(tmp, "deploy.json")
	if err := WriteMetadata(context.Background(), outputPath); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	var dm api.DeployManifest
	if err := json.Unmarshal(data, &dm); err != nil {
		t.Fatal(err)
	}
	pushOps, err := dm.PushOperations()
	if err != nil {
		t.Fatal(err)
	}
	layers := pushOps[0].Manifests[0].LayerBlobs
	if layers[0].Digest != testLayerDigestA || layers[0].BLAKE3Hex() != testLayerBLAKE3 {
		t.Errorf("layer 0 = %+v, want sha256 %s with blake3 %s", layers[0].Descriptor, testLayerDigestA, testLayerBLAKE3)
	}
	if layers[1].Digest != testLayerDigestB || layers[1].CASDigests != nil {
		t.Errorf("layer 1 = %+v, want sha256 %s and no cas digests", layers[1].Descriptor, testLayerDigestB)
	}
}
