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
