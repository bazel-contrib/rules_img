// Command extent-stream inspects and reconstructs extent streams (.xstream),
// an EXPERIMENTAL format that describes a byte sequence (a disk image, for
// example) as data, holes and references to blobs and to other streams; see
// docs/extent-stream.md.
//
// Subcommands:
//
//	reconstruct   rebuild the content from a stream and content-addressed directories
//	list (ls)     print a stream's header and extents without reconstruction
package extentstreamcmd

import (
	"context"
	"fmt"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/extentstream"
)

const usage = `Usage: img extent-stream <subcommand> [args...]

The extent stream format is experimental and may change incompatibly.

Subcommands:
  reconstruct   rebuilds the content of a stream from content-addressed directories
  list, ls      prints a stream's header and extents without reconstruction`

// ExtentStreamProcess dispatches to an extent-stream subcommand.
func ExtentStreamProcess(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, extentstream.ExperimentalNotice)
	switch args[0] {
	case "reconstruct":
		reconstructProcess(ctx, args[1:])
	case "list", "ls":
		listProcess(args[1:])
	case "help", "-h", "--help":
		fmt.Fprintln(os.Stderr, usage)
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand %q\n\n%s\n", args[0], usage)
		os.Exit(1)
	}
}
