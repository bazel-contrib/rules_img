// reconstruct rebuilds a layer tar from a compact stream (.cstream) and a
// content-addressed directory.
//
// Each CAS reference in the index is addressed by the sha256 of its content and
// is resolved by opening <cas-dir>/sha256/<first two hex digits>/<hex>. The content-addressed directory
// is produced by `img cas-dir` from the files that went into the layer; with
// several --cas-dir flags, a blob comes from the first directory that has it.
// The reconstructed tar is written to --output, which may be "-" for stdout.
package compactstreamcmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

func reconstructProcess(ctx context.Context, args []string) {
	var indexPath string
	var outputPath string
	var casDirs stringsFlag

	flagSet := flag.NewFlagSet("compact-stream reconstruct", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Reconstructs a layer tar from a compact stream and a content-addressed directory.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img compact-stream reconstruct --compact-stream <cstream> --cas-dir <dir> --output <tar|->\n")
		flagSet.PrintDefaults()
	}
	flagSet.StringVar(&indexPath, "compact-stream", "", "Path to the compact stream (.cstream) (required)")
	flagSet.Var(&casDirs, "cas-dir", "Content-addressed directory (containing sha256/<hex>) that provides CAS blobs (required). Can be repeated: a blob is taken from the first directory that has it.")
	flagSet.StringVar(&outputPath, "output", "", "Path to write the reconstructed tar, or \"-\" for stdout (required)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if indexPath == "" || outputPath == "" || len(casDirs) == 0 {
		fmt.Fprintf(os.Stderr, "Error: --compact-stream, --cas-dir and --output are required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	indexFile, err := os.Open(indexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening compact stream %s: %v\n", indexPath, err)
		os.Exit(1)
	}
	defer indexFile.Close()

	var output io.Writer
	var outputFile *os.File
	if outputPath == "-" {
		output = os.Stdout
	} else {
		outputFile, err = os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error opening output %s: %v\n", outputPath, err)
			os.Exit(1)
		}
		output = outputFile
	}

	if err := compactstream.Reconstruct(ctx, indexFile, compactstream.NewDirStore(casDirs...), output); err != nil {
		if outputFile != nil {
			// Reconstruction failed, so the output is partial/corrupt; surface a
			// close error too, but keep the reconstruction error as the primary one.
			if cerr := outputFile.Close(); cerr != nil {
				fmt.Fprintf(os.Stderr, "Error closing output %s: %v\n", outputPath, cerr)
			}
		}
		fmt.Fprintf(os.Stderr, "Error reconstructing tar from compact stream: %v\n", err)
		os.Exit(1)
	}
	if outputFile != nil {
		if err := outputFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "Error closing output %s: %v\n", outputPath, err)
			os.Exit(1)
		}
	}
}

// stringsFlag is a flag that can be given several times.
type stringsFlag []string

func (f *stringsFlag) String() string { return strings.Join(*f, ",") }

func (f *stringsFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}
