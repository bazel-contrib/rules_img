package deploymetadata

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

const loadTestManifest = `{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "` + testConfigDigest + `", "size": 42},
  "layers": [
    {"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": "` + testLayerDigestA + `", "size": 100}
  ]
}`

// resetMetadataFlags resets the package-level flag state that WriteMetadata
// reads, so each test observes a clean slate.
func resetMetadataFlags(manifestPath, configPath string) {
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
}

func writeLoadMetadata(t *testing.T, configJSON string) api.LoadDeployOperation {
	t.Helper()
	tmp := t.TempDir()

	manifestPath := filepath.Join(tmp, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(loadTestManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(tmp, "config.json")
	if err := os.WriteFile(configPath, []byte(configJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	command = "load"
	resetMetadataFlags(manifestPath, configPath)

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
		t.Fatalf("unmarshalling deploy manifest: %v", err)
	}
	ops, err := dm.LoadOperations()
	if err != nil {
		t.Fatalf("LoadOperations: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d load operations, want 1", len(ops))
	}
	return ops[0].LoadDeployOperation
}

func TestLoadOperationTagsOnly(t *testing.T) {
	op := writeLoadMetadata(t, `{"tags":["my-app:latest"],"daemon":"docker"}`)
	if op.Registry != "" || op.Repository != "" {
		t.Errorf("tags-only load op should have empty registry/repository, got %q / %q", op.Registry, op.Repository)
	}
	if !reflect.DeepEqual(op.Tags, []string{"my-app:latest"}) {
		t.Errorf("op.Tags = %v", op.Tags)
	}
	if got := op.ImageNames(); !reflect.DeepEqual(got, []string{"my-app:latest"}) {
		t.Errorf("ImageNames() = %v", got)
	}
}

func TestLoadOperationWithRegistryRepository(t *testing.T) {
	op := writeLoadMetadata(t, `{"registry":"gcr.io","repository":"proj/app","tags":["latest","v1"],"daemon":"containerd"}`)
	if op.Registry != "gcr.io" || op.Repository != "proj/app" {
		t.Errorf("registry/repository = %q / %q, want gcr.io / proj/app", op.Registry, op.Repository)
	}
	if op.Daemon != "containerd" {
		t.Errorf("daemon = %q, want containerd", op.Daemon)
	}
	want := []string{"gcr.io/proj/app:latest", "gcr.io/proj/app:v1"}
	if got := op.ImageNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("ImageNames() = %v, want %v", got, want)
	}
}

// TestLoadOperationLoneRegistryErrors verifies that a registry without a
// repository (e.g. a template that expanded to empty) is a hard error rather
// than a silent fallback to verbatim mode.
func TestLoadOperationLoneRegistryErrors(t *testing.T) {
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(loadTestManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(tmp, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"registry":"gcr.io","repository":"","tags":["latest"],"daemon":"docker"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	command = "load"
	resetMetadataFlags(manifestPath, configPath)

	if err := WriteMetadata(context.Background(), filepath.Join(tmp, "deploy.json")); err == nil {
		t.Fatal("expected error for registry without repository, got nil")
	}
}
