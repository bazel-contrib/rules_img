// Command generate_golden writes the exhaustive ndjson manifest of a layer tar
// to a file. It is meant to be run via `bazel run` to (re)generate the golden
// manifests consumed by layer_contents_test:
//
//	bazel run //tests/layering/generate_golden -- path/to/layer.tar path/to/golden.ndjson
//
// Relative paths are resolved against the directory `bazel run` was invoked from
// (BUILD_WORKING_DIRECTORY), so they are relative to your shell's working
// directory, not the runfiles tree. The layer may be gzip, zstd, or
// uncompressed; the format is detected automatically.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bazel-contrib/rules_img/tests/layering/manifest"
)

func main() {
	args := os.Args[1:]
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: generate_golden <layer.tar> <golden.ndjson>")
		os.Exit(2)
	}

	layerPath := resolve(args[0])
	goldenPath := resolve(args[1])

	out, err := os.Create(goldenPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating %s: %v\n", goldenPath, err)
		os.Exit(1)
	}
	if err := manifest.Dump(layerPath, out); err != nil {
		out.Close()
		fmt.Fprintf(os.Stderr, "generating manifest for %s: %v\n", layerPath, err)
		os.Exit(1)
	}
	if err := out.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "closing %s: %v\n", goldenPath, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", goldenPath)
}

// resolve makes a relative path relative to the directory `bazel run` was
// invoked from, falling back to the process working directory.
func resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if wd := os.Getenv("BUILD_WORKING_DIRECTORY"); wd != "" {
		return filepath.Join(wd, path)
	}
	return path
}
