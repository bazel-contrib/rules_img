package deploymetadata

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

const (
	testLayerDigestA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	testLayerDigestB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	testConfigDigest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

func TestParseLayerSources(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "layer_sources.json")
	// Matches the shape produced by push_metadata.bzl's json.encode.
	content := `{"0":[[{"registry":"index.docker.io","repository":"library/ubuntu"}],[]],"1":[[{"registry":"gcr.io","repository":"team/base"},{"registry":"mirror.example.com","repository":"team/base"}]]}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	layerSourcesForManifest = nil
	if err := parseLayerSources(path); err != nil {
		t.Fatalf("parseLayerSources: %v", err)
	}

	if got := sourcesForLayer(0, 0); !reflect.DeepEqual(got, []api.LayerSource{{Registry: "index.docker.io", Repository: "library/ubuntu"}}) {
		t.Errorf("sourcesForLayer(0,0) = %+v", got)
	}
	if got := sourcesForLayer(0, 1); len(got) != 0 {
		t.Errorf("sourcesForLayer(0,1) = %+v, want empty", got)
	}
	if got := sourcesForLayer(1, 0); len(got) != 2 {
		t.Errorf("sourcesForLayer(1,0) = %+v, want 2 sources", got)
	}
	// Out-of-range and unknown manifest indices return nil rather than panicking.
	if got := sourcesForLayer(0, 5); got != nil {
		t.Errorf("sourcesForLayer(0,5) = %+v, want nil", got)
	}
	if got := sourcesForLayer(9, 0); got != nil {
		t.Errorf("sourcesForLayer(9,0) = %+v, want nil", got)
	}
}

// TestWriteMetadataEmbedsLayerSources exercises the full Starlark->tool boundary:
// a layer-sources side file is attached to each layer's descriptor inside
// layer_blobs of the generated deploy manifest.
func TestWriteMetadataEmbedsLayerSources(t *testing.T) {
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
	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(tmp, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"registry":"registry.example.com","repository":"team/app","tags":["latest"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	sourcesPath := filepath.Join(tmp, "layer_sources.json")
	if err := os.WriteFile(sourcesPath, []byte(`{"0":[[{"registry":"index.docker.io","repository":"library/ubuntu"}],[]]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reset package-level flag state that WriteMetadata reads.
	command = "push"
	strategy = "eager"
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
	if err := parseLayerSources(sourcesPath); err != nil {
		t.Fatalf("parseLayerSources: %v", err)
	}

	outputPath := filepath.Join(tmp, "deploy.json")
	if err := WriteMetadata(context.Background(), outputPath); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"sources"`) {
		t.Fatalf("deploy manifest does not contain a sources field:\n%s", data)
	}

	var dm api.DeployManifest
	if err := json.Unmarshal(data, &dm); err != nil {
		t.Fatalf("unmarshalling deploy manifest: %v", err)
	}
	pushOps, err := dm.PushOperations()
	if err != nil {
		t.Fatalf("PushOperations: %v", err)
	}
	if len(pushOps) != 1 {
		t.Fatalf("got %d push operations, want 1", len(pushOps))
	}
	manifests := pushOps[0].Manifests
	if len(manifests) != 1 {
		t.Fatalf("got %d manifests, want 1", len(manifests))
	}
	layers := manifests[0].LayerBlobs
	if len(layers) != 2 {
		t.Fatalf("got %d layer blobs, want 2", len(layers))
	}
	if layers[0].Digest != testLayerDigestA {
		t.Errorf("layer[0].Digest = %s, want %s", layers[0].Digest, testLayerDigestA)
	}
	wantSources := []api.LayerSource{{Registry: "index.docker.io", Repository: "library/ubuntu"}}
	if !reflect.DeepEqual(layers[0].Sources, wantSources) {
		t.Errorf("layer[0].Sources = %+v, want %+v", layers[0].Sources, wantSources)
	}
	if len(layers[1].Sources) != 0 {
		t.Errorf("layer[1].Sources = %+v, want empty (omitted)", layers[1].Sources)
	}
}
