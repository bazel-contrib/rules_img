// Command verifier validates container image layer tar files against exhaustive
// ndjson manifests, one manifest per layer.
//
// It takes alternating <layer> <manifest> positional arguments (absolute paths;
// the hermetic launcher resolves them from runfiles) and, for each pair, iterates
// the layer tar and the manifest in lockstep (see the manifest package), failing
// on the first entry that does not match or if the entry counts differ.
//
// When the environment variable RULES_IMG_LAYERING_RECONSTRUCT_COMPACT_STREAM is set,
// each layer is additionally reconstructed from its compact stream by invoking
// `img compact-stream reconstruct`, and the result is asserted to be byte-for-byte
// identical to <layer>. The per-layer compact stream and the content-addressed input
// directory are located in the runfiles at "<prefix>/<layer index>"; the img tool
// is located via its embedded runfiles path (IMGRlocationPath).
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/bazel-contrib/rules_img/tests/layering/manifest"
)

// IMGRlocationPath is the runfiles path of the img tool, set at build time via
// the go_binary x_defs. It is only used when reconstruction is requested.
var IMGRlocationPath string

const reconstructEnv = "RULES_IMG_LAYERING_RECONSTRUCT_COMPACT_STREAM"

// These must stay in sync with the constants in //tests/layering:defs.bzl.
const (
	compactStreamRunfilesPrefix = "++rules_img_private++/compactstream"
	inputFileCASRunfilesPrefix  = "++rules_img_private++/inputfilecas"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 || len(args)%2 != 0 {
		fmt.Fprintln(os.Stderr, "usage: verifier <layer> <manifest> [<layer> <manifest> ...]")
		os.Exit(2)
	}

	reconstruct := os.Getenv(reconstructEnv) != ""
	var imgPath string
	if reconstruct {
		p, err := runfiles.Rlocation(IMGRlocationPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: locating img tool (%q): %v\n", IMGRlocationPath, err)
			os.Exit(1)
		}
		imgPath = p
	}

	for i := 0; i < len(args); i += 2 {
		blob, manifestPath := args[i], args[i+1]
		layerIdx := i / 2
		if err := manifest.VerifyLayer(blob, manifestPath); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: layer %d (%s): %v\n", layerIdx, blob, err)
			os.Exit(1)
		}
		if reconstruct {
			if err := verifyReconstruction(imgPath, layerIdx, blob); err != nil {
				fmt.Fprintf(os.Stderr, "FAIL: layer %d compact stream reconstruction does not match the layer built directly: %v\n", layerIdx, err)
				os.Exit(1)
			}
		}
	}
}

// verifyReconstruction reconstructs layer layerIdx from its compact stream and
// content-addressed input directory (both located in the runfiles) and checks
// that the reconstructed stream equals the layer blob byte-for-byte.
func verifyReconstruction(imgPath string, layerIdx int, blob string) error {
	compactStream, err := runfiles.Rlocation(compactStreamRunfilesPrefix + "/" + strconv.Itoa(layerIdx))
	if err != nil {
		return fmt.Errorf("locating compact stream: %w", err)
	}
	casDir, err := runfiles.Rlocation(inputFileCASRunfilesPrefix + "/" + strconv.Itoa(layerIdx))
	if err != nil {
		return fmt.Errorf("locating input file CAS directory: %w", err)
	}

	var stdout bytes.Buffer
	cmd := exec.Command(
		imgPath,
		"compact-stream",
		"reconstruct",
		"--compact-stream", compactStream,
		"--cas-dir", casDir,
		"--output", "-",
	)
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running img compact-stream reconstruct: %w", err)
	}

	expected, err := os.ReadFile(blob)
	if err != nil {
		return fmt.Errorf("reading layer blob %s: %w", blob, err)
	}
	got := stdout.Bytes()
	if len(got) != len(expected) {
		return fmt.Errorf("size mismatch: reconstructed %d bytes, layer blob %s is %d bytes", len(got), blob, len(expected))
	}
	if !bytes.Equal(got, expected) {
		for i := range got {
			if got[i] != expected[i] {
				return fmt.Errorf("byte mismatch at offset %d: reconstructed 0x%02x, layer blob 0x%02x", i, got[i], expected[i])
			}
		}
	}
	return nil
}
