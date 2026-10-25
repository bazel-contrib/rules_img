package extentstreamcmd

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/extentstream"
)

// stringsFlag is a flag that can be given several times.
type stringsFlag []string

func (f *stringsFlag) String() string { return strings.Join(*f, ",") }

func (f *stringsFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func reconstructProcess(ctx context.Context, args []string) {
	var streamPath, outputPath string
	var casDirs stringsFlag

	flagSet := flag.NewFlagSet("extent-stream reconstruct", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Reconstructs the content of an extent stream.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img extent-stream reconstruct --extent-stream <xstream> --cas-dir <dir> --output <file|->\n")
		flagSet.PrintDefaults()
	}
	flagSet.StringVar(&streamPath, "extent-stream", "", "Path to the extent stream (.xstream) (required)")
	flagSet.Var(&casDirs, "cas-dir", "Content-addressed directory (sha256/<hex>) with the blobs and the streams the stream references (required). Can be repeated: a blob is taken from the first directory that has it.")
	flagSet.StringVar(&outputPath, "output", "", "Path to write the content to, or \"-\" for stdout (required). A file stays sparse where the stream has holes.")
	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if streamPath == "" || outputPath == "" || len(casDirs) == 0 {
		fmt.Fprintf(os.Stderr, "Error: --extent-stream, --cas-dir and --output are required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if err := reconstruct(ctx, streamPath, outputPath, casDirs); err != nil {
		fmt.Fprintf(os.Stderr, "Error reconstructing from extent stream: %v\n", err)
		os.Exit(1)
	}
}

func reconstruct(ctx context.Context, streamPath, outputPath string, casDirs []string) error {
	in, err := os.Open(streamPath)
	if err != nil {
		return err
	}
	defer in.Close()
	store := compactstream.NewDirStore(casDirs...)
	if outputPath == "-" {
		return extentstream.Reconstruct(ctx, in, store, os.Stdout)
	}
	out, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	fw := &fileWriter{f: out}
	if err := extentstream.Reconstruct(ctx, in, store, fw); err != nil {
		_ = out.Close()
		return err
	}
	// Seeking over a trailing hole does not extend the file.
	if err := out.Truncate(fw.pos); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// fileWriter writes to a file and seeks over holes, so that the file is sparse.
type fileWriter struct {
	f   *os.File
	pos int64
}

func (w *fileWriter) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.pos += int64(n)
	return n, err
}

func (w *fileWriter) WriteHole(n int64) error {
	if _, err := w.f.Seek(n, io.SeekCurrent); err != nil {
		return err
	}
	w.pos += n
	return nil
}
