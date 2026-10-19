package deploymetadata

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

const ociManifestMediaType = "application/vnd.oci.image.manifest.v1+json"

// noMediaTypeManifest is loadTestManifest without its top-level mediaType, as
// image_manifest writes it with omit_media_type.
const noMediaTypeManifest = `{
  "schemaVersion": 2,
  "config": {"mediaType": "application/vnd.cncf.helm.config.v1+json", "digest": "` + testConfigDigest + `", "size": 42},
  "layers": [
    {"mediaType": "application/vnd.cncf.helm.chart.content.v1.tar+gzip", "digest": "` + testLayerDigestA + `", "size": 100}
  ]
}`

func TestWriteMetadataManifestWithoutMediaType(t *testing.T) {
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "manifest.json")
	if err := os.WriteFile(manifestPath, []byte(noMediaTypeManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(tmp, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"tags":["chart:1.0.0"],"daemon":"docker"}`), 0o644); err != nil {
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
	op := ops[0]
	if op.Root.MediaType != ociManifestMediaType {
		t.Errorf("root mediaType = %q, want %s", op.Root.MediaType, ociManifestMediaType)
	}
	if len(op.Manifests) != 1 {
		t.Fatalf("got %d manifests, want 1", len(op.Manifests))
	}
	if got := op.Manifests[0].Descriptor.MediaType; got != ociManifestMediaType {
		t.Errorf("manifest descriptor mediaType = %q, want %s", got, ociManifestMediaType)
	}
	if got := op.Manifests[0].Config.MediaType; got != "application/vnd.cncf.helm.config.v1+json" {
		t.Errorf("config mediaType = %q, want the manifest's config media type", got)
	}
}

func TestProcessReferrersManifestWithoutMediaType(t *testing.T) {
	const subjectDigest = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	referrer := `{"schemaVersion":2,"config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"` + testConfigDigest + `","size":2},"layers":[],"subject":{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + subjectDigest + `","size":10}}`
	tmp := t.TempDir()
	referrerPath := filepath.Join(tmp, "referrer.json")
	if err := os.WriteFile(referrerPath, []byte(referrer), 0o644); err != nil {
		t.Fatal(err)
	}

	destinationFilePath = ""
	referrerRootPaths = newIndexedStringFlag()
	referrerRootKinds = newIndexedStringFlag()
	referrerRunfilesSlots = newIndexedStringFlag()
	referrerManifestPaths = newDoubleIndexedStringFlag()
	referrerRootPaths.values[0] = referrerPath
	referrerRootKinds.values[0] = "manifest"
	referrerRunfilesSlots.values[0] = "slot"
	referrerManifestPaths.values[0] = map[int]string{0: referrerPath}

	config := map[string]any{"registry": "example.com", "repository": "test/app"}
	ops, err := processReferrers(map[string]bool{subjectDigest: true}, config, nil)
	if err != nil {
		t.Fatalf("processReferrers: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d referrer operations, want 1", len(ops))
	}
	var op api.PushDeployOperation
	if err := json.Unmarshal(ops[0], &op); err != nil {
		t.Fatal(err)
	}
	if op.Root.MediaType != ociManifestMediaType {
		t.Errorf("referrer root mediaType = %q, want %s", op.Root.MediaType, ociManifestMediaType)
	}
	if len(op.Manifests) != 1 || op.Manifests[0].Descriptor.MediaType != ociManifestMediaType {
		t.Errorf("referrer manifests = %+v, want one with mediaType %s", op.Manifests, ociManifestMediaType)
	}
}
