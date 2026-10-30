// Package mtree implements the `img mtree` subcommand, which writes an mtree(8)
// specification describing the metadata of one or more layers.
//
// Inputs are given by repeatable --tar, --cstream, --erofs, --erofs-cstream,
// --ext4, --ext4-cstream, --fat32, --fat32-cstream, --cpio, --cpio-cstream and --mtree flags and are processed in the order they appear on the command line
// (they may be interleaved). A tar blob (--tar) may be gzip/zstd-compressed; a
// compact stream (--cstream) is reconstructed with zero-filled bodies and its
// content digests come from the CAS reference table / inlined bytes; an EROFS
// layer blob (--erofs) is walked as a filesystem image; an EROFS layer's compact
// stream (--erofs-cstream) is walked as a sparse image whose payloads read as
// zeros, with the content digests again coming from the reference table; an
// ext4 layer (--ext4) and its compact stream (--ext4-cstream) are handled the
// same way, and so are a FAT32 layer (--fat32) and its compact stream
// (--fat32-cstream), and so is a cpio layer (--cpio, which may be gzip/zstd-compressed)
// and its compact stream (--cpio-cstream); an mtree spec (--mtree) is folded in with its keywords filtered to the requested
// field set and its paths re-normalized. The path layout, field set, and layer
// layout are controlled by --path-prefix, --options, and --layout.
package mtree

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"errors"
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
	erofsKind
	erofsCstreamKind
	ext4Kind
	ext4CstreamKind
	fat32Kind
	fat32CstreamKind
	cpioKind
	cpioCstreamKind
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
		fmt.Fprintf(flagSet.Output(), "Usage: img mtree (--tar <tar> | --cstream <cstream> | --erofs <blob> | --erofs-cstream <cstream> | --ext4 <image> | --ext4-cstream <cstream> | --fat32 <image> | --fat32-cstream <cstream> | --cpio <archive> | --cpio-cstream <cstream> | --mtree <mtree>)... --output <mtree|->\n")
		flagSet.PrintDefaults()
		fmt.Fprintf(flagSet.Output(), "\nInputs are processed in command-line order and may be interleaved.\n")
	}
	flagSet.Var(orderedInput{tarKind, &inputs}, "tar", "Add a layer tar blob input (may be gzip- or zstd-compressed). Repeatable.")
	flagSet.Var(orderedInput{cstreamKind, &inputs}, "cstream", "Add a compact stream (.cstream) input. Repeatable.")
	flagSet.Var(orderedInput{erofsKind, &inputs}, "erofs", "Add an EROFS layer blob input (may be zstd-compressed). Repeatable.")
	flagSet.Var(orderedInput{erofsCstreamKind, &inputs}, "erofs-cstream", "Add the compact stream of an EROFS layer, read as a sparse image whose file payloads are absent. Repeatable.")
	flagSet.Var(orderedInput{ext4Kind, &inputs}, "ext4", "Add an ext4 layer image input. Repeatable.")
	flagSet.Var(orderedInput{ext4CstreamKind, &inputs}, "ext4-cstream", "Add the compact stream of an ext4 layer, read as a sparse image whose file payloads are absent. Repeatable.")
	flagSet.Var(orderedInput{fat32Kind, &inputs}, "fat32", "Add a FAT32 layer image input. Repeatable.")
	flagSet.Var(orderedInput{fat32CstreamKind, &inputs}, "fat32-cstream", "Add the compact stream of a FAT32 layer, read as a sparse image whose file payloads are absent. Repeatable.")
	flagSet.Var(orderedInput{cpioKind, &inputs}, "cpio", "Add a cpio layer input (may be gzip/zstd-compressed). Repeatable.")
	flagSet.Var(orderedInput{cpioCstreamKind, &inputs}, "cpio-cstream", "Add the compact stream of a cpio layer, read as an archive whose file payloads are absent. Repeatable.")
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
		fmt.Fprintf(os.Stderr, "Error: at least one of --tar, --cstream, --erofs, --erofs-cstream, --ext4, --ext4-cstream, --fat32, --fat32-cstream, --cpio, --cpio-cstream, or --mtree is required\n")
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
		case erofsKind:
			image, imageCloser, ierr := filesystemImage(f)
			if ierr != nil {
				return fmt.Errorf("reading EROFS layer %s: %w", ref.path, ierr)
			}
			closers = append(closers, imageCloser)
			inputs = append(inputs, mtree.Input{Kind: mtree.ErofsInput, ReaderAt: image, Digester: mtree.HashContent})
		case erofsCstreamKind:
			sparse, serr := compactstream.NewSparseReaderAt(f)
			if serr != nil {
				return fmt.Errorf("reading compact stream %s: %w", ref.path, serr)
			}
			inputs = append(inputs, mtree.Input{
				Kind:          mtree.ErofsInput,
				ReaderAt:      sparse,
				Digester:      mtree.HashContent,
				RangeDigester: sparse.RefDigestInRange,
			})
		case ext4Kind:
			image, imageCloser, ierr := filesystemImage(f)
			if ierr != nil {
				return fmt.Errorf("reading ext4 layer %s: %w", ref.path, ierr)
			}
			closers = append(closers, imageCloser)
			inputs = append(inputs, mtree.Input{Kind: mtree.Ext4Input, ReaderAt: image, Digester: mtree.HashContent})
		case ext4CstreamKind:
			sparse, serr := compactstream.NewSparseReaderAt(f)
			if serr != nil {
				return fmt.Errorf("reading compact stream %s: %w", ref.path, serr)
			}
			inputs = append(inputs, mtree.Input{
				Kind:          mtree.Ext4Input,
				ReaderAt:      sparse,
				Digester:      mtree.HashContent,
				RangeDigester: sparse.RefDigestInRange,
			})
		case fat32Kind:
			image, imageCloser, ierr := filesystemImage(f)
			if ierr != nil {
				return fmt.Errorf("reading FAT32 layer %s: %w", ref.path, ierr)
			}
			closers = append(closers, imageCloser)
			inputs = append(inputs, mtree.Input{Kind: mtree.Fat32Input, ReaderAt: image, Digester: mtree.HashContent})
		case fat32CstreamKind:
			sparse, serr := compactstream.NewSparseReaderAt(f)
			if serr != nil {
				return fmt.Errorf("reading compact stream %s: %w", ref.path, serr)
			}
			inputs = append(inputs, mtree.Input{
				Kind:          mtree.Fat32Input,
				ReaderAt:      sparse,
				Digester:      mtree.HashContent,
				RangeDigester: sparse.RefDigestInRange,
			})
		case cpioKind:
			archive, archiveCloser, ierr := filesystemImage(f)
			if ierr != nil {
				return fmt.Errorf("reading cpio layer %s: %w", ref.path, ierr)
			}
			closers = append(closers, archiveCloser)
			inputs = append(inputs, mtree.Input{Kind: mtree.CpioInput, ReaderAt: archive, Digester: mtree.HashContent})
		case cpioCstreamKind:
			sparse, serr := compactstream.NewSparseReaderAt(f)
			if serr != nil {
				return fmt.Errorf("reading compact stream %s: %w", ref.path, serr)
			}
			inputs = append(inputs, mtree.Input{
				Kind:          mtree.CpioInput,
				ReaderAt:      sparse,
				Digester:      mtree.HashContent,
				RangeDigester: sparse.RefDigestInRange,
			})
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

// filesystemImage returns random access to a filesystem image layer blob (EROFS
// ext4 or FAT32) or a cpio layer, which walking a filesystem image requires (the tar path streams
// because a tar can only be read in order anyway). An uncompressed blob is
// addressed in place. A compressed one is decompressed into a temporary file
// rather than into memory, so the cost does not grow with the layer; the
// returned closer removes it.
func filesystemImage(f *os.File) (io.ReaderAt, io.Closer, error) {
	var magic [4]byte
	n, err := f.ReadAt(magic[:], 0)
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	gzipped := n >= 2 && magic[0] == 0x1f && magic[1] == 0x8b
	zstded := n >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd
	if !gzipped && !zstded {
		return f, closerFunc(func() error { return nil }), nil
	}

	uncompressed, err := mtree.Decompress(io.NewSectionReader(f, 0, 1<<62))
	if err != nil {
		return nil, nil, err
	}
	tmp, err := os.CreateTemp("", "fs-image-layer-*")
	if err != nil {
		return nil, nil, err
	}
	closer := closerFunc(func() error {
		return errors.Join(tmp.Close(), os.Remove(tmp.Name()))
	})
	if _, err := io.Copy(tmp, uncompressed); err != nil {
		_ = closer()
		return nil, nil, err
	}
	return tmp, closer, nil
}
