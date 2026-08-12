package deploymetadata

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// writePushMetadata runs the deploy-metadata writer over one push operation and
// returns it, so a test can check what the build recorded for `img deploy` to read.
func writePushMetadata(t *testing.T, configJSON string) api.PushDeployOperation {
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

	command = "push"
	resetMetadataFlags(manifestPath, configPath)
	t.Cleanup(func() { resetMetadataFlags(manifestPath, configPath) })

	outputPath := filepath.Join(tmp, "deploy.json")
	deduplicatedPush = api.DeduplicatedPushBestEffort
	deduplicatedPushBlobRepository = "team/_blobs"
	deduplicatedPushContent = api.DeduplicatedPushContentBlobsAndArtificialManifests
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
	ops, err := dm.PushOperations()
	if err != nil {
		t.Fatalf("PushOperations: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("got %d push operations, want 1", len(ops))
	}
	return ops[0].PushDeployOperation
}

// TestPushOperationRecordsDeduplicatedPushSettings covers the three deduplicated push
// settings travelling with the operation rather than with the deploy: one deploy can
// merge operations headed for registries that need different treatment.
func TestPushOperationRecordsDeduplicatedPushSettings(t *testing.T) {
	op := writePushMetadata(t, `{"registry":"reg.example.com","repository":"team/app","tags":["latest"]}`)

	if op.DeduplicatedPush != api.DeduplicatedPushBestEffort {
		t.Errorf("deduplicated_push = %q, want %q", op.DeduplicatedPush, api.DeduplicatedPushBestEffort)
	}
	if op.DeduplicatedPushBlobRepository != "team/_blobs" {
		t.Errorf("deduplicated_push_blob_repository = %q, want team/_blobs", op.DeduplicatedPushBlobRepository)
	}
	if op.DeduplicatedPushContent != api.DeduplicatedPushContentBlobsAndArtificialManifests {
		t.Errorf("deduplicated_push_content = %q, want %q", op.DeduplicatedPushContent, api.DeduplicatedPushContentBlobsAndArtificialManifests)
	}
}
