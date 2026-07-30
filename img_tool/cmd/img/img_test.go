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
		name         string
		args         []string
		wantArgs     []string
		wantInsecure bool
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			previous := registryopts.Insecure()
			registryopts.SetInsecure(false)
			t.Cleanup(func() { registryopts.SetInsecure(previous) })

			got := handleGlobalFlags(tc.args)
			if !slices.Equal(got, tc.wantArgs) {
				t.Errorf("handleGlobalFlags(%v) = %v, want %v", tc.args, got, tc.wantArgs)
			}
			if registryopts.Insecure() != tc.wantInsecure {
				t.Errorf("insecure mode = %v, want %v", registryopts.Insecure(), tc.wantInsecure)
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

	handleGlobalFlags([]string{"img", "deploy", "--request-file", "req.json"})
	if !registryopts.Insecure() {
		t.Error("insecure mode was disabled by args that don't mention --insecure")
	}
}
