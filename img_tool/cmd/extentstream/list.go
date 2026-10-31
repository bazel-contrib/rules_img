package extentstreamcmd

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/extentstream"
)

func listProcess(args []string) {
	flagSet := flag.NewFlagSet("extent-stream list", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Prints the header and the extents of an extent stream.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img extent-stream list <xstream>\n")
	}
	if err := flagSet.Parse(args); err != nil || flagSet.NArg() != 1 {
		flagSet.Usage()
		os.Exit(1)
	}
	if err := list(flagSet.Arg(0), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func list(path string, out *os.File) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	idx, err := extentstream.Inspect(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	h := idx.Header
	fmt.Fprintf(out, "Extent stream (experimental), version %d\n", h.Version)
	fmt.Fprintf(out, "  Content format: %s\n", h.ContentFormat)
	fmt.Fprintf(out, "  Size:           %d bytes\n", h.Size)
	fmt.Fprintf(out, "  Extents:        %d\n", h.ExtentCount)
	if h.HasContentDigest {
		fmt.Fprintf(out, "  Content digest: sha256:%x\n", h.ContentDigest)
	}
	var data, holes, refs uint64
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "\nOFFSET\tLENGTH\tKIND\tSOURCE")
	var pos uint64
	for _, e := range idx.Extents {
		src := ""
		switch e.Kind {
		case extentstream.KindData:
			data += e.Length
		case extentstream.KindHole:
			holes += e.Length
		default:
			refs += e.Length
			src = fmt.Sprintf("sha256:%x (%d bytes) from %d", e.Digest, e.BlobSize, e.SrcOffset)
		}
		fmt.Fprintf(tw, "%d\t%d\t%s\t%s\n", pos, e.Length, e.Kind, src)
		pos += e.Length
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nEmbedded: %d bytes, holes: %d bytes, referenced: %d bytes\n", data, holes, refs)
	return nil
}
