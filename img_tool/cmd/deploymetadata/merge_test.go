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

func TestDeployMergeProcessExpandsArgfile(t *testing.T) {
	tmp := t.TempDir()
	input1 := writeDeployManifest(t, filepath.Join(tmp, "input1.json"), "first")
	input2 := writeDeployManifest(t, filepath.Join(tmp, "input2.json"), "second")
	output := filepath.Join(tmp, "out.json")
	argfile := filepath.Join(tmp, "args.params")

	args := []string{
		"--push-strategy",
		"lazy",
		"--load-strategy",
		"eager",
		input1,
		input2,
		output,
	}
	if err := os.WriteFile(argfile, []byte(strings.Join(args, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("writing argfile: %v", err)
	}

	DeployMergeProcess(context.Background(), []string{"@" + argfile})

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("reading output manifest: %v", err)
	}

	var got api.DeployManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshalling output manifest: %v", err)
	}

	if got.Settings.PushStrategy != "lazy" {
		t.Errorf("PushStrategy = %q, want lazy", got.Settings.PushStrategy)
	}
	if got.Settings.LoadStrategy != "eager" {
		t.Errorf("LoadStrategy = %q, want eager", got.Settings.LoadStrategy)
	}

	var commands []string
	for _, raw := range got.Operations {
		var op struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(raw, &op); err != nil {
			t.Fatalf("unmarshalling operation %s: %v", raw, err)
		}
		commands = append(commands, op.Command)
	}

	wantCommands := []string{"first", "second"}
	if !reflect.DeepEqual(commands, wantCommands) {
		t.Errorf("merged commands = %v, want %v", commands, wantCommands)
	}
}

func TestMergeDeployManifestsFiltersByOperation(t *testing.T) {
	tmp := t.TempDir()
	// A manifest carrying every operation kind we filter on. registry_tag rides
	// along with push; referrer operations also use the "push" command.
	input := writeDeployManifest(t, filepath.Join(tmp, "in.json"), "push", "registry_tag", "load")

	cases := []struct {
		name       string
		operations []string
		want       []string
	}{
		{"push only", []string{"push"}, []string{"push", "registry_tag"}},
		{"load only", []string{"load"}, []string{"load"}},
		{"both", []string{"push", "load"}, []string{"push", "registry_tag", "load"}},
		{"no filter keeps all", nil, []string{"push", "registry_tag", "load"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := filepath.Join(tmp, tc.name+".json")
			if err := MergeDeployManifests(context.Background(), []string{input}, output, tc.operations); err != nil {
				t.Fatalf("MergeDeployManifests: %v", err)
			}
			if got := mergedCommands(t, output); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("filtered commands = %v, want %v", got, tc.want)
			}
		})
	}
}

// An unrecognized command must never be dropped, even when a filter is active.
func TestMergeDeployManifestsKeepsUnknownCommands(t *testing.T) {
	tmp := t.TempDir()
	input := writeDeployManifest(t, filepath.Join(tmp, "in.json"), "push", "mystery", "load")
	output := filepath.Join(tmp, "out.json")

	if err := MergeDeployManifests(context.Background(), []string{input}, output, []string{"load"}); err != nil {
		t.Fatalf("MergeDeployManifests: %v", err)
	}
	if got, want := mergedCommands(t, output), []string{"mystery", "load"}; !reflect.DeepEqual(got, want) {
		t.Errorf("filtered commands = %v, want %v", got, want)
	}
}

// TestMergeDeployManifestsPreservesSettings verifies that the top-level settings
// which are not reconstructed from the strategy flags -- BlobRepository,
// ForbidLayerPush, and DefaultSignSetting -- are carried through the merge from
// the input manifests. Regression test: dropping them let a merged deploy silently
// re-upload layers that forbid_layer_push was meant to block.
func TestMergeDeployManifestsPreservesSettings(t *testing.T) {
	tmp := t.TempDir()
	input1 := writeDeployManifestWithSettings(t, filepath.Join(tmp, "in1.json"), api.DeploySettings{
		BlobRepository:     "staging-blobs",
		ForbidLayerPush:    true,
		DefaultSignSetting: &api.Descriptor{MediaType: api.SignSettingMediaType, Digest: "sha256:abc", Size: 42},
	}, "push")
	// A second manifest with the same global-flag-driven values (the common case),
	// plus a load op that carries none of these settings.
	input2 := writeDeployManifestWithSettings(t, filepath.Join(tmp, "in2.json"), api.DeploySettings{
		BlobRepository:  "staging-blobs",
		ForbidLayerPush: true,
	}, "push", "load")
	output := filepath.Join(tmp, "out.json")

	if err := MergeDeployManifests(context.Background(), []string{input1, input2}, output, nil); err != nil {
		t.Fatalf("MergeDeployManifests: %v", err)
	}

	got := readManifest(t, output)
	if got.Settings.BlobRepository != "staging-blobs" {
		t.Errorf("BlobRepository = %q, want staging-blobs", got.Settings.BlobRepository)
	}
	if !got.Settings.ForbidLayerPush {
		t.Errorf("ForbidLayerPush = false, want true")
	}
	if got.Settings.DefaultSignSetting == nil || got.Settings.DefaultSignSetting.Digest != "sha256:abc" {
		t.Errorf("DefaultSignSetting = %+v, want digest sha256:abc", got.Settings.DefaultSignSetting)
	}
}

// TestMergeDeployManifestsForbidLayerPushORs verifies that ForbidLayerPush is
// OR-combined: if any input forbids layer push, the merged manifest does too.
func TestMergeDeployManifestsForbidLayerPushORs(t *testing.T) {
	tmp := t.TempDir()
	off := writeDeployManifestWithSettings(t, filepath.Join(tmp, "off.json"), api.DeploySettings{}, "push")
	on := writeDeployManifestWithSettings(t, filepath.Join(tmp, "on.json"), api.DeploySettings{ForbidLayerPush: true}, "push")
	output := filepath.Join(tmp, "out.json")

	if err := MergeDeployManifests(context.Background(), []string{off, on}, output, nil); err != nil {
		t.Fatalf("MergeDeployManifests: %v", err)
	}
	if !readManifest(t, output).Settings.ForbidLayerPush {
		t.Errorf("ForbidLayerPush = false, want true (OR of inputs)")
	}
}

// TestMergeDeployManifestsConflictingBlobRepository verifies that merging inputs
// with divergent blob_repository values fails loudly rather than silently picking
// one (a single top-level field cannot represent both).
func TestMergeDeployManifestsConflictingBlobRepository(t *testing.T) {
	tmp := t.TempDir()
	a := writeDeployManifestWithSettings(t, filepath.Join(tmp, "a.json"), api.DeploySettings{BlobRepository: "repo-a"}, "push")
	b := writeDeployManifestWithSettings(t, filepath.Join(tmp, "b.json"), api.DeploySettings{BlobRepository: "repo-b"}, "push")
	output := filepath.Join(tmp, "out.json")

	err := MergeDeployManifests(context.Background(), []string{a, b}, output, nil)
	if err == nil {
		t.Fatalf("MergeDeployManifests: expected error for conflicting blob_repository, got nil")
	}
	if !strings.Contains(err.Error(), "blob_repository") {
		t.Errorf("error = %q, want it to mention blob_repository", err)
	}
}

// readManifest reads and unmarshals a deploy manifest for assertions.
func readManifest(t *testing.T, path string) api.DeployManifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading output manifest: %v", err)
	}
	var got api.DeployManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshalling output manifest: %v", err)
	}
	return got
}

// writeDeployManifestWithSettings writes a deploy manifest carrying the given
// settings and one operation per command.
func writeDeployManifestWithSettings(t *testing.T, path string, settings api.DeploySettings, commands ...string) string {
	t.Helper()
	manifest := api.DeployManifest{Settings: settings}
	for _, command := range commands {
		raw, err := json.Marshal(struct {
			Command string `json:"command"`
		}{Command: command})
		if err != nil {
			t.Fatalf("marshalling operation: %v", err)
		}
		manifest.Operations = append(manifest.Operations, raw)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshalling deploy manifest: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing deploy manifest: %v", err)
	}
	return path
}

// mergedCommands reads a merged deploy manifest and returns the command of each
// operation in order.
func mergedCommands(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading output manifest: %v", err)
	}
	var got api.DeployManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshalling output manifest: %v", err)
	}
	var commands []string
	for _, raw := range got.Operations {
		var op struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(raw, &op); err != nil {
			t.Fatalf("unmarshalling operation %s: %v", raw, err)
		}
		commands = append(commands, op.Command)
	}
	return commands
}

func writeDeployManifest(t *testing.T, path string, commands ...string) string {
	t.Helper()

	manifest := api.DeployManifest{
		Operations: make([]json.RawMessage, 0, len(commands)),
	}
	for _, command := range commands {
		raw, err := json.Marshal(struct {
			Command string `json:"command"`
		}{
			Command: command,
		})
		if err != nil {
			t.Fatalf("marshalling operation: %v", err)
		}
		manifest.Operations = append(manifest.Operations, raw)
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshalling deploy manifest: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing deploy manifest: %v", err)
	}
	return path
}
