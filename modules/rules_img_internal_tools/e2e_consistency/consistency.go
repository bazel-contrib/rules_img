// Package main implements a consistency checker/fixer ensuring that every
// deployable push/load operation in an e2e workspace (image_push, image_load,
// and image_manifest/image_index carrying push_specs/load_specs) is referenced
// by that workspace's root multi_deploy deploy/push/load targets, so newly added
// push/load targets are never forgotten by the integration test flow.
//
//   - `bazel test //util/e2e_consistency:consistency_test` fails on drift.
//   - `bazel run //util/e2e_consistency -- --fix` rewrites the root BUILD files
//     to add the missing operations (creating the deploy/push/load targets when
//     absent).
//
// A push/load target opts out by carrying the tag "no-multi-deploy".
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bazelbuild/buildtools/build"
)

const optOutTag = "no-multi-deploy"

const multiDeployBzl = "@rules_img//img:multi_deploy.bzl"

// classify reports whether a rule is a deployable push and/or load operation.
//
//   - image_push -> push; image_load -> load.
//   - image_manifest / image_index contribute a push and/or load operation only
//     when they carry push_specs / load_specs (they provide DeployInfo then).
//   - image_push_spec / image_load_spec are spec *definitions*, not operations,
//     so they are ignored (the image target that references them is the op).
func classify(r *build.Rule) (isPush, isLoad bool) {
	switch r.Kind() {
	case "image_push":
		return true, false
	case "image_load":
		return false, true
	case "image_manifest", "image_index":
		return len(r.AttrStrings("push_specs")) > 0, len(r.AttrStrings("load_specs")) > 0
	}
	return false, false
}

// deployTargets are the three standardized root targets: which operation labels
// each includes and the deploy_operations filter it should carry.
var deployTargets = []struct {
	name        string
	includePush bool
	includeLoad bool
	deployOps   []string // deploy_operations value; nil => omit (defaults to both)
}{
	{name: "deploy", includePush: true, includeLoad: true, deployOps: nil},
	{name: "push", includePush: true, includeLoad: false, deployOps: []string{"push"}},
	{name: "load", includePush: false, includeLoad: true, deployOps: []string{"load"}},
}

// scanResult holds the push/load operation labels discovered in a workspace and
// the parsed root BUILD file.
type scanResult struct {
	root       string   // workspace root dir
	rootBuild  string   // path to the root BUILD.bazel
	pushLabels []string // sorted, workspace-relative labels ("//pkg:name")
	loadLabels []string
}

// scanWorkspace walks a workspace, collecting non-opted-out push/load target
// labels (workspace-relative) and locating the root BUILD file.
func scanWorkspace(wsRoot string) (*scanResult, error) {
	res := &scanResult{root: wsRoot}
	var pushSet, loadSet []string

	err := filepath.WalkDir(wsRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip the copied-in scratch and bazel symlink trees, if any.
			base := d.Name()
			if base == "bazel-out" || strings.HasPrefix(base, "bazel-") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "BUILD.bazel" && d.Name() != "BUILD" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f, err := build.ParseBuild(path, data)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		pkg, err := filepath.Rel(wsRoot, filepath.Dir(path))
		if err != nil {
			return err
		}
		if pkg == "." {
			pkg = ""
		}
		pkg = filepath.ToSlash(pkg)
		if filepath.Base(path) == "BUILD.bazel" && pkg == "" {
			res.rootBuild = path
		}
		for _, r := range f.Rules("") {
			isPush, isLoad := classify(r)
			if !isPush && !isLoad {
				continue
			}
			if hasTag(r, optOutTag) {
				continue
			}
			label := "//" + pkg + ":" + r.Name()
			if isPush {
				pushSet = append(pushSet, label)
			}
			if isLoad {
				loadSet = append(loadSet, label)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if res.rootBuild == "" {
		res.rootBuild = filepath.Join(wsRoot, "BUILD.bazel")
	}
	sort.Strings(pushSet)
	sort.Strings(loadSet)
	res.pushLabels = pushSet
	res.loadLabels = loadSet
	return res, nil
}

func hasTag(r *build.Rule, tag string) bool {
	for _, t := range r.AttrStrings("tags") {
		if t == tag {
			return true
		}
	}
	return false
}

// normalizeLabel canonicalizes a workspace-relative label so shorthand forms
// compare equal: ":x" -> "//:x", "//pkg" -> "//pkg:pkg". Absolute forms with a
// colon are returned unchanged.
func normalizeLabel(raw string) string {
	if strings.HasPrefix(raw, ":") {
		return "//" + raw
	}
	if strings.HasPrefix(raw, "//") {
		if strings.Contains(raw, ":") {
			return raw
		}
		// "//pkg" == "//pkg:pkg"; "//" == "//:" (degenerate, unused).
		pkg := strings.TrimPrefix(raw, "//")
		name := pkg
		if idx := strings.LastIndex(pkg, "/"); idx >= 0 {
			name = pkg[idx+1:]
		}
		return raw + ":" + name
	}
	return raw
}

func normalizeAll(labels []string) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, normalizeLabel(l))
	}
	return out
}

// wantedOperations returns the operation labels the given deploy target should
// carry for this workspace.
func (s *scanResult) wantedOperations(includePush, includeLoad bool) []string {
	var out []string
	if includePush {
		out = append(out, s.pushLabels...)
	}
	if includeLoad {
		out = append(out, s.loadLabels...)
	}
	sort.Strings(out)
	return dedupe(out)
}

func dedupe(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// checkWorkspace returns human-readable drift messages for a workspace.
//
// A workspace is only enforced once it has adopted the pattern (defines at least
// one of the deploy/push/load multi_deploy targets). This keeps the check green
// during incremental rollout while still catching a push/load target that was
// added to an adopted workspace but forgotten in its deploy targets.
func checkWorkspace(name string, s *scanResult) []string {
	var drift []string
	data, err := os.ReadFile(s.rootBuild)
	if err != nil {
		return []string{fmt.Sprintf("%s: reading root BUILD: %v", name, err)}
	}
	f, err := build.ParseBuild(s.rootBuild, data)
	if err != nil {
		return []string{fmt.Sprintf("%s: parsing root BUILD: %v", name, err)}
	}
	if !isAdopted(f) {
		return nil
	}
	for _, dt := range deployTargets {
		want := s.wantedOperations(dt.includePush, dt.includeLoad)
		rule := findRule(f, "multi_deploy", dt.name)
		if rule == nil {
			if len(want) == 0 {
				continue
			}
			drift = append(drift, fmt.Sprintf("%s: missing multi_deploy(name = %q) with operations %v", name, dt.name, want))
			continue
		}
		got := rule.AttrStrings("operations")
		gotSet := toSet(normalizeAll(got))
		var missing []string
		for _, w := range want {
			if !gotSet[w] {
				missing = append(missing, w)
			}
		}
		if len(missing) > 0 {
			drift = append(drift, fmt.Sprintf("%s: //:%s is missing operations %v", name, dt.name, missing))
		}
	}
	return drift
}

// isAdopted reports whether the root BUILD defines any of the standardized
// deploy/push/load multi_deploy targets.
func isAdopted(f *build.File) bool {
	for _, dt := range deployTargets {
		if findRule(f, "multi_deploy", dt.name) != nil {
			return true
		}
	}
	return false
}

// fixWorkspace rewrites the workspace root BUILD to add missing operations,
// creating the deploy/push/load targets when absent. Returns whether it changed.
func fixWorkspace(name string, s *scanResult) (bool, error) {
	data, err := os.ReadFile(s.rootBuild)
	if err != nil {
		return false, err
	}
	f, err := build.ParseBuild(s.rootBuild, data)
	if err != nil {
		return false, err
	}
	changed := false
	for _, dt := range deployTargets {
		want := s.wantedOperations(dt.includePush, dt.includeLoad)
		if len(want) == 0 {
			continue
		}
		rule := findRule(f, "multi_deploy", dt.name)
		if rule == nil {
			f.Stmt = append(f.Stmt, newMultiDeploy(dt.name, want, dt.deployOps).Call)
			changed = true
			continue
		}
		merged := union(normalizeAll(rule.AttrStrings("operations")), want)
		if !equalStrings(merged, normalizeAll(rule.AttrStrings("operations"))) {
			rule.SetAttr("operations", stringList(merged))
			changed = true
		}
	}
	if changed {
		ensureMultiDeployLoad(f)
		if err := os.WriteFile(s.rootBuild, build.Format(f), 0o644); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func findRule(f *build.File, kind, name string) *build.Rule {
	for _, r := range f.Rules(kind) {
		if r.Name() == name {
			return r
		}
	}
	return nil
}

func newMultiDeploy(name string, operations, deployOps []string) *build.Rule {
	call := &build.CallExpr{X: &build.Ident{Name: "multi_deploy"}}
	call.List = append(call.List, assign("name", &build.StringExpr{Value: name}))
	call.List = append(call.List, assign("operations", stringList(operations)))
	if deployOps != nil {
		call.List = append(call.List, assign("deploy_operations", stringList(deployOps)))
	}
	return &build.Rule{Call: call}
}

func assign(name string, rhs build.Expr) *build.AssignExpr {
	return &build.AssignExpr{LHS: &build.Ident{Name: name}, Op: "=", RHS: rhs}
}

func stringList(values []string) *build.ListExpr {
	list := &build.ListExpr{ForceMultiLine: true}
	for _, v := range values {
		list.List = append(list.List, &build.StringExpr{Value: v})
	}
	return list
}

// ensureMultiDeployLoad makes sure multi_deploy is loaded from its .bzl.
func ensureMultiDeployLoad(f *build.File) {
	for _, stmt := range f.Stmt {
		load, ok := stmt.(*build.LoadStmt)
		if !ok || load.Module == nil || load.Module.Value != multiDeployBzl {
			continue
		}
		for _, to := range load.To {
			if to.Name == "multi_deploy" {
				return
			}
		}
	}
	load := &build.LoadStmt{
		Module:       &build.StringExpr{Value: multiDeployBzl},
		From:         []*build.Ident{{Name: "multi_deploy"}},
		To:           []*build.Ident{{Name: "multi_deploy"}},
		ForceCompact: true,
	}
	// Insert after any existing leading load statements for tidy output.
	insertAt := 0
	for i, stmt := range f.Stmt {
		if _, ok := stmt.(*build.LoadStmt); ok {
			insertAt = i + 1
		}
	}
	rest := append([]build.Expr{load}, f.Stmt[insertAt:]...)
	f.Stmt = append(f.Stmt[:insertAt], rest...)
}

func toSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return set
}

func union(a, b []string) []string {
	set := toSet(a)
	out := append([]string{}, a...)
	for _, v := range b {
		if !set[v] {
			out = append(out, v)
			set[v] = true
		}
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string{}, a...)
	sb := append([]string{}, b...)
	sort.Strings(sa)
	sort.Strings(sb)
	for i := range sa {
		if sa[i] != sb[i] {
			return false
		}
	}
	return true
}
