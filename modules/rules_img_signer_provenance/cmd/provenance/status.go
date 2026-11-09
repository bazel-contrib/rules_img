package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Keys of a status command's output that the plugin understands. They follow the
// BUILD_SCM_* convention used by Bazel workspace status scripts.
const (
	// statusRevision is the commit of the source (configSource.digest and
	// buildkit_metadata.vcs.revision).
	statusRevision = "BUILD_SCM_REVISION"
	// statusURL is the URI of the source repository (configSource.uri and
	// buildkit_metadata.vcs.source).
	statusURL = "BUILD_SCM_URL"
	// statusRef is the git ref the source was at (bazel definitions only).
	statusRef = "BUILD_SCM_REF"
	// statusPath is the path of the build definition inside the source
	// (configSource.path).
	statusPath = "BUILD_SCM_PATH"
	// statusState says whether the working tree was modified. Anything but
	// "clean" (or empty) marks vcs.revision with "-dirty", as buildx does.
	statusState = "BUILD_SCM_STATUS"

	statusTimeout = time.Minute
)

// workspaceStatus is the parsed output of a status command.
type workspaceStatus map[string]string

// runStatusCommand runs a Bazel-style workspace status command and parses its
// output. The command is executed directly (not through a shell) in the Bazel
// workspace when run by `bazel run`, so it can use git, and inherits the
// environment of the deploy. A failing command is an error: provenance with
// silently missing source information would be misleading.
func runStatusCommand(ctx context.Context, command string, args []string) (workspaceStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = os.Getenv("BUILD_WORKSPACE_DIRECTORY") // empty means the current directory
	cmd.Stderr = os.Stderr
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("status command %q failed: %w", command, err)
	}
	return parseStatus(&stdout)
}

// parseStatus reads Bazel's workspace status format: one "KEY VALUE" pair per
// line, split at the first space; the value may contain spaces or be empty. Blank
// lines are skipped and a later line wins over an earlier one with the same key.
func parseStatus(r io.Reader) (workspaceStatus, error) {
	status := workspaceStatus{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		if key == "" {
			return nil, fmt.Errorf("status output line %d has no key: %q", n, line)
		}
		status[key] = value
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading status output: %w", err)
	}
	return status, nil
}

// dirty reports whether the status marks the working tree as modified.
func (s workspaceStatus) dirty() bool {
	state := strings.TrimSpace(s[statusState])
	return state != "" && !strings.EqualFold(state, "clean")
}

// extras returns the allowlisted keys that are present, for
// internalParameters.workspace_status. Nothing else of the output is recorded:
// a status script may print values that must not end up in an attestation.
func (s workspaceStatus) extras(keys []string) map[string]string {
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := s[k]; ok {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
