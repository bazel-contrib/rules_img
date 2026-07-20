// Package pushcmd implements the `img push` command used by the "push at build
// time" feature. Unlike `img deploy` (which is driven by a
// `bazel run` launcher and its runfiles tree), these subcommands are invoked as
// Bazel build actions and take their inputs as explicit file paths.
//
// Subcommands:
//
//	push blob      pushes a single layer blob to a repository (optionally a
//	               staging repository) and records where it landed.
//	push manifest  pushes the config blob and manifest(s)/tags of an image,
//	               cross-mounting the layer blobs from where `push blob` put them.
package pushcmd

import (
	"context"
	"fmt"
	"os"
)

const usage = `Usage: img push <subcommand> [flags]

Subcommands:
  blob      Push a single layer blob to a (staging) repository.
  manifest  Push config + manifest(s) of an image, mounting already-pushed layers.
`

// PushProcess dispatches to the blob/manifest subcommands.
func PushProcess(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "blob":
		blobProcess(ctx, rest)
	case "manifest":
		manifestProcess(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "Unknown push subcommand %q\n\n%s", subcommand, usage)
		os.Exit(1)
	}
}

// BlobResult records where a single blob was pushed by `img push blob`. The
// manifest push reads these to know which repository to cross-mount each blob
// from.
type BlobResult struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
	MediaType  string `json:"mediaType,omitempty"`
	Size       int64  `json:"size,omitempty"`
}

// finish applies the best_effort/enabled failure semantics of the push-at-build-time
// feature. On success it writes the output file (a marker or a JSON result) and
// exits 0. On error: in "enabled" mode it logs and exits non-zero without writing
// the output (so the validation action fails the build); in "best_effort" mode it
// logs, still writes the output (so downstream actions can run), and exits 0.
func finish(mode, outputPath string, outputContents []byte, err error) {
	if err == nil {
		if writeErr := os.WriteFile(outputPath, outputContents, 0o644); writeErr != nil {
			fmt.Fprintf(os.Stderr, "Error writing output %s: %v\n", outputPath, writeErr)
			os.Exit(1)
		}
		return
	}
	switch mode {
	case "best_effort":
		fmt.Fprintf(os.Stderr, "WARNING: push at build time failed (best_effort, build continues): %v\n", err)
		if writeErr := os.WriteFile(outputPath, outputContents, 0o644); writeErr != nil {
			fmt.Fprintf(os.Stderr, "Error writing output %s: %v\n", outputPath, writeErr)
			os.Exit(1)
		}
		return
	default: // "enabled" (and any unknown mode) fails the build.
		fmt.Fprintf(os.Stderr, "Error: push at build time failed: %v\n", err)
		os.Exit(1)
	}
}
