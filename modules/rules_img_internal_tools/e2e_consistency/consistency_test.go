package main

import (
	"path/filepath"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

// TestE2EDeployConsistency fails if any e2e workspace has an image_push/
// image_load (or *_spec) target that is not referenced by that workspace's root
// deploy/push/load multi_deploy targets. Repair with:
//
//	bazel run //util/e2e_consistency -- --fix
func TestE2EDeployConsistency(t *testing.T) {
	rootBuild, err := runfiles.Rlocation("_main/e2e/BUILD.bazel")
	if err != nil {
		t.Fatalf("locating e2e dir via runfiles: %v", err)
	}
	e2eDir := filepath.Dir(rootBuild)

	workspaces, err := discoverWorkspaces(e2eDir)
	if err != nil {
		t.Fatalf("discovering workspaces: %v", err)
	}
	if len(workspaces) == 0 {
		t.Fatalf("no e2e workspaces found under %s", e2eDir)
	}

	var drift []string
	for _, name := range workspaces {
		res, err := scanWorkspace(filepath.Join(e2eDir, name))
		if err != nil {
			t.Fatalf("scanning %s: %v", name, err)
		}
		drift = append(drift, checkWorkspace(name, res)...)
	}
	for _, d := range drift {
		t.Errorf("drift: %s", d)
	}
	if len(drift) > 0 {
		t.Log("run `bazel run //util/e2e_consistency -- --fix` to repair")
	}
}
