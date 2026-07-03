// Package mtree implements the `img mtree` subcommand, which writes an mtree(8)
// specification describing the metadata of one or more layers.
//
// Inputs are given by repeatable --tar, --cstream, and --mtree flags and are
// processed in the order they appear on the command line (they may be
// interleaved). A tar blob (--tar) may be gzip/zstd-compressed; a compact stream
// (--cstream) is reconstructed with zero-filled bodies and its content digests
// come from the CAS reference table / inlined bytes; an mtree spec (--mtree) is
// folded in with its keywords filtered to the requested field set and its paths
// re-normalized. The path layout, field set, and layer layout are controlled by
// --path-prefix, --options, and --layout.
package mtree

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/mtree"
)

// inputRef is one --tar/--cstream/--mtree input, in command-line order.
type inputRef struct {
	kind inputKind
	path string
}

type inputKind int

const (
	tarKind inputKind = iota
	cstreamKind
	mtreeKind
)

// orderedInput is a flag.Value that appends each occurrence of an input flag to
// a shared slice, so the relative order of --tar/--cstream/--mtree across the
// command line is preserved (flag.Parse calls Set in argument order).
type orderedInput struct {
	kind   inputKind
	inputs *[]inputRef
}

func (o orderedInput) String() string { return "" }

func (o orderedInput) Set(v string) error {
	*o.inputs = append(*o.inputs, inputRef{kind: o.kind, path: v})
	return nil
}

func MtreeProcess(ctx context.Context, args []string) {
	var inputs []inputRef
	var outputPath string
	defaults := mtree.DefaultOptions()
	var pathPrefix string
	var options string
	var layout string

	flagSet := flag.NewFlagSet("mtree", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Writes an mtree specification of one or more layers' metadata.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img mtree (--tar <tar> | --cstream <cstream> | --mtree <mtree>)... --output <mtree|->\n")
		flagSet.PrintDefaults()
		fmt.Fprintf(flagSet.Output(), "\nInputs are processed in command-line order and may be interleaved.\n")
	}
	flagSet.Var(orderedInput{tarKind, &inputs}, "tar", "Add a layer tar blob input (may be gzip- or zstd-compressed). Repeatable.")
	flagSet.Var(orderedInput{cstreamKind, &inputs}, "cstream", "Add a compact stream (.cstream) input. Repeatable.")
	flagSet.Var(orderedInput{mtreeKind, &inputs}, "mtree", "Add an existing mtree spec input. Repeatable.")
	flagSet.StringVar(&outputPath, "output", "", `Path to write the mtree to, or "-" for stdout (required).`)
	flagSet.StringVar(&pathPrefix, "path-prefix", defaults.PathPrefix, `Prefix for entry paths: "./" (full-path entries) or "" (bare tar paths).`)
	flagSet.StringVar(&options, "options", strings.Join(defaults.Keywords, ","), "Comma-separated, ordered list of fields to emit (type,size,mode,uid,uname,gid,gname,sha256,time,link,nlink,xattr).")
	flagSet.StringVar(&layout, "layout", defaults.Layout, "Layer layout: \"tar\" or \"oci_layer_filesystem_applied_changeset\".")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	if len(inputs) == 0 {
		fmt.Fprintf(os.Stderr, "Error: at least one of --tar, --cstream, or --mtree is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	if outputPath == "" {
		fmt.Fprintf(os.Stderr, "Error: --output is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	opts := mtree.Options{
		PathPrefix: pathPrefix,
		Keywords:   splitOptions(options),
		Layout:     layout,
	}

	if err := run(ctx, inputs, opts, outputPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing mtree: %v\n", err)
		os.Exit(1)
	}
}

// run opens every input, assembles the ordered mtree.Input list, and renders it.
// All inputs are opened up front and closed at the end because WriteMulti streams
// through them in order.
func run(ctx context.Context, refs []inputRef, opts mtree.Options, outputPath string) (err error) {
	var closers []io.Closer
	defer func() {
		for i := len(closers) - 1; i >= 0; i-- {
			if cerr := closers[i].Close(); cerr != nil && err == nil {
				err = cerr
			}
		}
	}()

	var inputs []mtree.Input
	for _, ref := range refs {
		f, oerr := os.Open(ref.path)
		if oerr != nil {
			return fmt.Errorf("opening %s: %w", ref.path, oerr)
		}
		closers = append(closers, f)

		switch ref.kind {
		case tarKind:
			uncompressed, derr := mtree.Decompress(f)
			if derr != nil {
				return derr
			}
			inputs = append(inputs, mtree.Input{Kind: mtree.TarInput, Reader: uncompressed, Digester: mtree.HashContent})
		case cstreamKind:
			reader, rerr := compactstream.NewReconstructingReader(ctx, f, compactstream.NullBlobStore{})
			if rerr != nil {
				return rerr
			}
			closers = append(closers, reader)
			inputs = append(inputs, mtree.Input{Kind: mtree.TarInput, Reader: reader, Digester: cstreamDigester(reader)})
		case mtreeKind:
			inputs = append(inputs, mtree.Input{Kind: mtree.MtreeInput, Reader: f})
		}
	}

	out, closeOut, oerr := openOutput(outputPath)
	if oerr != nil {
		return fmt.Errorf("opening output %s: %w", outputPath, oerr)
	}
	closers = append(closers, closerFunc(closeOut))

	return mtree.WriteMulti(out, opts, inputs)
}

// cstreamDigester returns a ContentDigester for a compact-stream reader: a
// CAS-referenced file uses the digest recorded in the ref table (the content
// sha256, not read from the zero-filled body); an inlined file is hashed from
// the content present in the stream.
func cstreamDigester(reader *compactstream.ReconstructingReader) mtree.ContentDigester {
	return func(hdr *tar.Header, content io.Reader) ([]byte, error) {
		if digest, ok := reader.RefDigestAt(reader.Offset(), hdr.Size); ok {
			return digest, nil
		}
		h := sha256.New()
		if _, err := io.Copy(h, content); err != nil {
			return nil, err
		}
		return h.Sum(nil), nil
	}
}

// splitOptions parses the comma-separated --options value into a trimmed,
// non-empty field list.
func splitOptions(options string) []string {
	var out []string
	for _, part := range strings.Split(options, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// openOutput returns the writer for outputPath (or stdout for "-") and a close
// function that is a no-op for stdout.
func openOutput(outputPath string) (io.Writer, func() error, error) {
	if outputPath == "-" {
		return os.Stdout, func() error { return nil }, nil
	}
	f, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, nil, err
	}
	return f, f.Close, nil
}

type closerFunc func() error

func (c closerFunc) Close() error { return c() }
