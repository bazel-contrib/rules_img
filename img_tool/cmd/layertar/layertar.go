// Package layertar implements the `img layer-tar` subcommand, which writes the
// files of a layer, whatever its format, as one uncompressed tar stream.
//
// It is the format-neutral view of a layer for tools that only read tar, such
// as an SBOM scanner: a tar layer (gzip/zstd-compressed or not) is decompressed,
// a compact stream is reconstructed from its content-addressed directory, and an
// EROFS, ext4, FAT32 or ISO 9660 filesystem image or a cpio archive is walked
// and re-rendered as tar entries (EROFS whiteouts become ".wh." markers, as in a
// tar changeset). The layer's format comes from its media type.
package layertar

import (
	"archive/tar"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/mtree"
)

type stringsFlag []string

func (s *stringsFlag) String() string { return strings.Join(*s, ",") }

func (s *stringsFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// LayerTarProcess is the entry point of `img layer-tar`.
func LayerTarProcess(ctx context.Context, args []string) {
	var mediaType, blobPath, cstreamPath, outputPath string
	var casDirs stringsFlag

	flagSet := flag.NewFlagSet("layer-tar", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Writes the files of a layer, in any layer format, as an uncompressed tar.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img layer-tar --media-type <type> (--blob <blob> | --compact-stream <cstream> --cas-dir <dir>...) --output <tar|->\n")
		flagSet.PrintDefaults()
	}
	flagSet.StringVar(&mediaType, "media-type", "", "Media type of the layer, which decides how it is read (a tar unless it names an EROFS, ext4, FAT32, ISO 9660 or cpio layer).")
	flagSet.StringVar(&blobPath, "blob", "", "Path to the layer blob.")
	flagSet.StringVar(&cstreamPath, "compact-stream", "", "Path to the layer's compact stream (.cstream), instead of its blob.")
	flagSet.Var(&casDirs, "cas-dir", "Content-addressed directory with the content a compact stream refers to. Repeatable.")
	flagSet.StringVar(&outputPath, "output", "", `Path to write the tar to, or "-" for stdout (required).`)
	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if outputPath == "" || (blobPath == "") == (cstreamPath == "") {
		fmt.Fprintln(os.Stderr, "Error: --output and exactly one of --blob and --compact-stream are required")
		flagSet.Usage()
		os.Exit(1)
	}
	if cstreamPath != "" && len(casDirs) == 0 {
		fmt.Fprintln(os.Stderr, "Error: --compact-stream needs at least one --cas-dir")
		os.Exit(1)
	}

	if err := run(ctx, mediaType, blobPath, cstreamPath, casDirs, outputPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing the layer as a tar: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, mediaType, blobPath, cstreamPath string, casDirs []string, outputPath string) (err error) {
	var out io.Writer = os.Stdout
	if outputPath != "-" {
		f, err := os.Create(outputPath)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, f.Close()) }()
		out = f
	}
	if cstreamPath != "" {
		return writeCompactStream(ctx, out, Kind(mediaType), cstreamPath, casDirs)
	}
	return writeBlob(out, Kind(mediaType), blobPath)
}

// Kind returns how a layer with the given media type is read: as a tar
// (mtree.TarInput), or as a filesystem image.
func Kind(mediaType string) mtree.InputKind {
	switch {
	case strings.Contains(mediaType, "erofs"):
		return mtree.ErofsInput
	case mediaType == "application/x-ext4":
		return mtree.Ext4Input
	case mediaType == "application/x-fat32":
		return mtree.Fat32Input
	case mediaType == "application/x-iso9660-image":
		return mtree.Iso9660Input
	case strings.HasPrefix(mediaType, "application/x-cpio"):
		return mtree.CpioInput
	default:
		return mtree.TarInput
	}
}

func writeBlob(out io.Writer, kind mtree.InputKind, blobPath string) error {
	f, err := os.Open(blobPath)
	if err != nil {
		return err
	}
	defer f.Close()
	if kind == mtree.TarInput {
		uncompressed, err := mtree.Decompress(f)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, uncompressed)
		return err
	}
	image, closer, err := mtree.OpenFilesystemImage(f)
	if err != nil {
		return err
	}
	defer closer.Close()
	return WriteFilesystem(out, kind, image)
}

func writeCompactStream(ctx context.Context, out io.Writer, kind mtree.InputKind, cstreamPath string, casDirs []string) error {
	f, err := os.Open(cstreamPath)
	if err != nil {
		return err
	}
	defer f.Close()
	store := compactstream.NewDirStore(casDirs...)
	if kind == mtree.TarInput {
		return compactstream.ReconstructUncompressed(ctx, f, store, out)
	}

	// A filesystem image is read at random, so it is reconstructed into a
	// temporary file first.
	tmp, err := os.CreateTemp("", "layer-tar-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if err := compactstream.ReconstructUncompressed(ctx, f, store, tmp); err != nil {
		return err
	}
	return WriteFilesystem(out, kind, tmp)
}

// WriteFilesystem renders the entries of a filesystem image layer as an
// uncompressed tar.
func WriteFilesystem(out io.Writer, kind mtree.InputKind, image io.ReaderAt) error {
	tw := tar.NewWriter(out)
	err := mtree.WalkFilesystem(kind, image, func(hdr *tar.Header, content io.Reader) error {
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("writing %q: %w", hdr.Name, err)
		}
		if content != nil {
			if _, err := io.Copy(tw, content); err != nil {
				return fmt.Errorf("writing %q: %w", hdr.Name, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
