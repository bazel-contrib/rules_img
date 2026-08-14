package main

import (
	"slices"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// TestHandleGlobalFlags verifies that the global flags are consumed before a
// subcommand's own flag parser sees them, and that --insecure switches the
// process into insecure-registry mode.
func TestHandleGlobalFlags(t *testing.T) {
	tests := []struct {
		name             string
		args             []string
		wantArgs         []string
		wantInsecure     bool
		wantInvocationID string
	}{
		{
			name:     "no global flags",
			args:     []string{"img", "deploy", "--request-file", "req.json"},
			wantArgs: []string{"img", "deploy", "--request-file", "req.json"},
		},
		{
			name:         "insecure before the subcommand",
			args:         []string{"img", "--insecure", "deploy", "--request-file", "req.json"},
			wantArgs:     []string{"img", "deploy", "--request-file", "req.json"},
			wantInsecure: true,
		},
		{
			name:         "insecure after the subcommand args",
			args:         []string{"img", "deploy", "--request-file", "req.json", "--insecure"},
			wantArgs:     []string{"img", "deploy", "--request-file", "req.json"},
			wantInsecure: true,
		},
		{
			name:         "single-dash spelling, mixed with verbose",
			args:         []string{"img", "-insecure", "push", "blob", "--verbose"},
			wantArgs:     []string{"img", "push", "blob"},
			wantInsecure: true,
		},
		{
			name:             "invocation ID after subcommand",
			args:             []string{"img", "deploy", "--request-file", "req.json", "--invocation-id=1234"},
			wantArgs:         []string{"img", "deploy", "--request-file", "req.json"},
			wantInvocationID: "1234",
		},
		{
			name:             "Bazel-style invocation ID spelling",
			args:             []string{"img", "--invocation_id", "5678", "deploy", "--request-file", "req.json"},
			wantArgs:         []string{"img", "deploy", "--request-file", "req.json"},
			wantInvocationID: "5678",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			previous := registryopts.Insecure()
			registryopts.SetInsecure(false)
			t.Cleanup(func() { registryopts.SetInsecure(previous) })

			got, opts, err := handleGlobalFlags(tc.args)
			if err != nil {
				t.Fatalf("handleGlobalFlags(%v): %v", tc.args, err)
			}
			if !slices.Equal(got, tc.wantArgs) {
				t.Errorf("handleGlobalFlags(%v) = %v, want %v", tc.args, got, tc.wantArgs)
			}
			if registryopts.Insecure() != tc.wantInsecure {
				t.Errorf("insecure mode = %v, want %v", registryopts.Insecure(), tc.wantInsecure)
			}
			if opts.toolInvocationID != tc.wantInvocationID {
				t.Errorf("tool invocation ID = %q, want %q", opts.toolInvocationID, tc.wantInvocationID)
			}
		})
	}
}

// TestHandleGlobalFlagsKeepsEnvInsecure documents that the flag is only ever
// enabling: its absence must not undo IMG_INSECURE.
func TestHandleGlobalFlagsKeepsEnvInsecure(t *testing.T) {
	previous := registryopts.Insecure()
	registryopts.SetInsecure(true)
	t.Cleanup(func() { registryopts.SetInsecure(previous) })

	if _, _, err := handleGlobalFlags([]string{"img", "deploy", "--request-file", "req.json"}); err != nil {
		t.Fatal(err)
	}
	if !registryopts.Insecure() {
		t.Error("insecure mode was disabled by args that don't mention --insecure")
	}
}

func TestHandleGlobalFlagsRejectsMissingInvocationID(t *testing.T) {
	for _, args := range [][]string{
		{"img", "deploy", "--invocation-id"},
		{"img", "deploy", "--invocation-id="},
	} {
		if _, _, err := handleGlobalFlags(args); err == nil {
			t.Fatalf("expected %v to fail", args)
		}
	}
}

func TestActionMnemonic(t *testing.T) {
	for command, want := range map[string]string{
		"deploy":            "ImgDeploy",
		"download-manifest": "ImgDownloadManifest",
		"bes":               "ImgBES",
	} {
		if got := actionMnemonic(command); got != want {
			t.Errorf("actionMnemonic(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestResolveToolInvocationID(t *testing.T) {
	t.Setenv("BUILD_ID", "bazel-invocation")
	if got := resolveToolInvocationID(globalOptions{}); got != "bazel-invocation" {
		t.Errorf("BUILD_ID fallback = %q, want bazel-invocation", got)
	}
	if got := resolveToolInvocationID(globalOptions{toolInvocationID: "explicit"}); got != "explicit" {
		t.Errorf("explicit invocation ID = %q, want explicit", got)
	}
}
