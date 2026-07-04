// Command compact-stream inspects and reconstructs compact streames (.cstream).
//
// A compact stream is a compact representation of a (compressed) tar stream in
// which contiguous ranges of the uncompressed data are replaced by
// content-addressed references; see docs/compact-stream.md for the on-disk format.
//
// Subcommands:
//
//	reconstruct   rebuild a layer tar from an index and a content-addressed directory
//	list (ls)     print an index's header, contents, and statistics without reconstruction
package compactstreamcmd

import (
	"context"
	"fmt"
	"os"
)

const usage = `Usage: img compact-stream <subcommand> [args...]

Subcommands:
  reconstruct   rebuilds a layer tar from a compact stream and a content-addressed directory
  list, ls      prints a compact stream's header, contents, and statistics without reconstruction`

// CompactStreamProcess dispatches to a compact-stream subcommand.
func CompactStreamProcess(ctx context.Context, args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}

	subcommand, rest := args[0], args[1:]
	switch subcommand {
	case "reconstruct":
		reconstructProcess(ctx, rest)
	case "list", "ls":
		listProcess(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "Unknown compact-stream subcommand %q\n\n", subcommand)
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
}
