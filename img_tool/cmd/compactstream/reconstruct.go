// reconstruct rebuilds a layer tar from a compact stream (.cstream) and a
// content-addressed directory.
//
// Each CAS reference in the index is addressed by the sha256 of its content and
// is resolved by opening <cas-dir>/sha256/<hex>. The content-addressed directory
// is produced by `img cas-dir` from the files that went into the layer. The
// reconstructed tar is written to --output, which may be "-" for stdout.
package compactstreamcmd

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

func reconstructProcess(ctx context.Context, args []string) {
	var indexPath string
	var outputPath string
	var casDir string

	flagSet := flag.NewFlagSet("compact-stream reconstruct", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Reconstructs a layer tar from a compact stream and a content-addressed directory.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img compact-stream reconstruct --compact-stream <cstream> --cas-dir <dir> --output <tar|->\n")
		flagSet.PrintDefaults()
	}
	flagSet.StringVar(&indexPath, "compact-stream", "", "Path to the compact stream (.cstream) (required)")
	flagSet.StringVar(&casDir, "cas-dir", "", "Content-addressed directory (containing sha256/<hex>) that provides CAS blobs (required)")
	flagSet.StringVar(&outputPath, "output", "", "Path to write the reconstructed tar, or \"-\" for stdout (required)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if indexPath == "" || outputPath == "" || casDir == "" {
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

	store := &dirStore{shaDir: filepath.Join(casDir, "sha256")}
	if err := compactstream.Reconstruct(ctx, indexFile, store, output); err != nil {
		if outputFile != nil {
			outputFile.Close()
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

// dirStore is a compactstream.BlobStore backed by a content-addressed directory, where
// each blob is stored at sha256/<hex of content>.
type dirStore struct {
	shaDir string
}

func (s *dirStore) ReaderForBlob(_ context.Context, digest []byte, size int64) (io.ReadCloser, error) {
	path := filepath.Join(s.shaDir, hex.EncodeToString(digest))
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("blob sha256:%s (size %d) not found in content-addressed directory: %w", hex.EncodeToString(digest), size, err)
	}
	return f, nil
}
