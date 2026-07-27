package soci

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/ztoc"
)

// ZtocProcess generates a ztoc (SOCI table of contents) for a single
// gzip-compressed tar layer blob. It is used to produce a ztoc "on the fly" for
// layers whose SingleLayerInfo did not already carry one (e.g. recompressed,
// optimized, or imported layers).
func ZtocProcess(_ context.Context, args []string) {
	var (
		blobPath            string
		outputPath          string
		spanSize            int64
		buildToolIdentifier string
	)

	flagSet := flag.NewFlagSet("ztoc", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Generates a ztoc (SOCI table of contents) for a gzip-compressed tar layer.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img ztoc --blob layer.tgz --output layer.ztoc [--span-size bytes]\n")
		flagSet.PrintDefaults()
		examples := []string{
			"img ztoc --blob layer.tgz --output layer.ztoc",
		}
		fmt.Fprintf(flagSet.Output(), "\nExamples:\n")
		for _, example := range examples {
			fmt.Fprintf(flagSet.Output(), "  $ %s\n", example)
		}
		os.Exit(1)
	}
	flagSet.StringVar(&blobPath, "blob", "", `The gzip-compressed tar layer blob to index.`)
	flagSet.StringVar(&outputPath, "output", "", `Output file for the ztoc.`)
	flagSet.Int64Var(&spanSize, "span-size", int64(ztoc.DefaultSpanSize), `Minimum number of uncompressed bytes between ztoc checkpoints.`)
	flagSet.StringVar(&buildToolIdentifier, "build-tool-identifier", ztoc.DefaultBuildToolIdentifier, `Recorded in the ztoc's build_tool_identifier field.`)

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if blobPath == "" || outputPath == "" {
		fmt.Fprintln(os.Stderr, "--blob and --output are required")
		flagSet.Usage()
		os.Exit(1)
	}
	if spanSize <= 0 {
		fmt.Fprintf(os.Stderr, "--span-size must be positive, got %d\n", spanSize)
		os.Exit(1)
	}

	z, err := ztoc.BuildFromFile(blobPath, ztoc.WithSpanSize(spanSize), ztoc.WithBuildToolIdentifier(buildToolIdentifier))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to build ztoc for %s: %v\n", blobPath, err)
		os.Exit(1)
	}
	data, err := ztoc.Marshal(z)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to marshal ztoc: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(outputPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to write ztoc to %s: %v\n", outputPath, err)
		os.Exit(1)
	}
}
