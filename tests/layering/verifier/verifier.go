// Command verifier validates container image layer tar files against exhaustive
// ndjson manifests, one manifest per layer.
//
// It takes alternating <layer> <manifest> positional arguments (absolute paths;
// the hermetic launcher resolves them from runfiles) and, for each pair,
// iterates the layer tar and the manifest in lockstep (see the manifest
// package), failing on the first entry that does not match or if the entry
// counts differ.
package main

import (
	"fmt"
	"os"

	"github.com/bazel-contrib/rules_img/tests/layering/manifest"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 || len(args)%2 != 0 {
		fmt.Fprintln(os.Stderr, "usage: verifier <layer> <manifest> [<layer> <manifest> ...]")
		os.Exit(2)
	}

	for i := 0; i < len(args); i += 2 {
		blob, manifestPath := args[i], args[i+1]
		if err := manifest.VerifyLayer(blob, manifestPath); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: layer %d (%s): %v\n", i/2, blob, err)
			os.Exit(1)
		}
	}
}
