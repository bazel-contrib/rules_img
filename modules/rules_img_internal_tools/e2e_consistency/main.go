package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("e2e_consistency", flag.ContinueOnError)
	fix := fs.Bool("fix", false, "Rewrite root BUILD files to add missing deploy/push/load operations.")
	e2eDir := fs.String("e2e-dir", "", "Path to the e2e directory (defaults to $BUILD_WORKSPACE_DIRECTORY/e2e).")
	only := fs.String("workspace", "", "Restrict to a single e2e workspace (e.g. \"js\"); default: all.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dir := *e2eDir
	if dir == "" {
		ws := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
		if ws == "" {
			fmt.Fprintln(os.Stderr, "e2e_consistency: --e2e-dir is required when not run via `bazel run`")
			return 2
		}
		dir = filepath.Join(ws, "e2e")
	}

	workspaces, err := discoverWorkspaces(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e_consistency: %v\n", err)
		return 1
	}
	if *only != "" {
		workspaces = filterWorkspaces(workspaces, *only)
		if len(workspaces) == 0 {
			fmt.Fprintf(os.Stderr, "e2e_consistency: no e2e workspace named %q\n", *only)
			return 1
		}
	}

	if *fix {
		return runFix(dir, workspaces)
	}
	return runCheck(dir, workspaces)
}

func filterWorkspaces(workspaces []string, only string) []string {
	for _, w := range workspaces {
		if w == only {
			return []string{w}
		}
	}
	return nil
}

// discoverWorkspaces returns the names of e2e subdirectories that are Bazel
// workspaces (contain MODULE.bazel or WORKSPACE.bazel).
func discoverWorkspaces(e2eDir string) ([]string, error) {
	entries, err := os.ReadDir(e2eDir)
	if err != nil {
		return nil, fmt.Errorf("reading e2e dir %s: %w", e2eDir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		root := filepath.Join(e2eDir, e.Name())
		if isWorkspace(root) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func isWorkspace(dir string) bool {
	for _, marker := range []string{"MODULE.bazel", "WORKSPACE.bazel", "WORKSPACE"} {
		if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
			return true
		}
	}
	return false
}

func runCheck(e2eDir string, workspaces []string) int {
	var drift []string
	for _, name := range workspaces {
		res, err := scanWorkspace(filepath.Join(e2eDir, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e_consistency: %v\n", err)
			return 1
		}
		drift = append(drift, checkWorkspace(name, res)...)
	}
	if len(drift) > 0 {
		fmt.Fprintf(os.Stderr, "e2e deploy/push/load targets are out of sync (%d issue(s)):\n", len(drift))
		for _, d := range drift {
			fmt.Fprintf(os.Stderr, "  - %s\n", d)
		}
		fmt.Fprintln(os.Stderr, "\nRun `bazel run //util/e2e_consistency -- --fix` to repair, or tag a target with \"no-multi-deploy\" to exclude it.")
		return 1
	}
	fmt.Printf("e2e deploy/push/load targets are consistent (%d workspace(s) checked)\n", len(workspaces))
	return 0
}

func runFix(e2eDir string, workspaces []string) int {
	changedAny := false
	for _, name := range workspaces {
		res, err := scanWorkspace(filepath.Join(e2eDir, name))
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e_consistency: %v\n", err)
			return 1
		}
		changed, err := fixWorkspace(name, res)
		if err != nil {
			fmt.Fprintf(os.Stderr, "e2e_consistency: fixing %s: %v\n", name, err)
			return 1
		}
		if changed {
			fmt.Printf("updated %s\n", res.rootBuild)
			changedAny = true
		}
	}
	if !changedAny {
		fmt.Println("e2e deploy/push/load targets already consistent; nothing to do")
	}
	return 0
}
