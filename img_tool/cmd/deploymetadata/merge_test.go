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
