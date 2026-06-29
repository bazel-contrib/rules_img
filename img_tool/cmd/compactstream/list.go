// list prints a human-readable view of a compact stream: its header, the
// contents in reconstruction order (interleaved inline stream segments and CAS
// references), and summary statistics — all without reconstructing the tar.
//
// When the index header records the compressed-stream digest and size (written
// by the layer tool), list reports the compressed layer size and its efficiency
// directly from the header, with no need to fetch CAS blobs.
package compactstreamcmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

func listProcess(_ context.Context, args []string) {
	flagSet := flag.NewFlagSet("compact-stream list", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Prints a compact stream's header, contents, and statistics without reconstruction.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img compact-stream list <compact-stream>\n")
		flagSet.PrintDefaults()
	}

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if flagSet.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "Error: exactly one compact stream path is required\n")
		flagSet.Usage()
		os.Exit(1)
	}
	indexPath := flagSet.Arg(0)

	indexBytes, err := os.ReadFile(indexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading compact stream %s: %v\n", indexPath, err)
		os.Exit(1)
	}

	info, err := compactstream.Inspect(bytes.NewReader(indexBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing compact stream %s: %v\n", indexPath, err)
		os.Exit(1)
	}

	writeListing(os.Stdout, indexPath, info, int64(len(indexBytes)))
}

// writeListing renders the index report. The compressed-layer size and its
// efficiency are reported only when the header records the compressed-stream
// digest and size.
func writeListing(w io.Writer, name string, info *compactstream.Info, indexSize int64) {
	h := info.Header

	fmt.Fprintf(w, "compact stream: %s\n\n", name)

	fmt.Fprintln(w, "Header")
	row(w, "Format:", fmt.Sprintf("compact stream, version %d", h.Version))
	row(w, "Hash algorithm:", fmt.Sprintf("%s (%d-byte digests)", hashAlgoName(h.HashAlgo), h.HashSize))
	row(w, "Stream compression:", streamCompressionName(h.StreamCompression))
	row(w, "Layer compression:", describeOriginalCompression(h.OriginalCompression))
	row(w, "Seekable (estargz):", yesNo(h.OriginalCompression.Seekable))
	row(w, "End padding:", humanizeBytes(uint64(h.OriginalCompression.EndPadding)))
	row(w, "Reference table:", fmt.Sprintf("%d entries at offset %d (%s)", h.RefCount(), h.RefTableOffset, humanizeBytes(h.RefTableSize)))
	row(w, "Byte stream:", fmt.Sprintf("%s on disk, at offset %d", humanizeBytes(h.StreamSize), h.StreamOffset))
	if h.HasCompressedStreamInfo {
		row(w, "Compressed stream digest:", fmt.Sprintf("%s:%s", hashAlgoName(h.HashAlgo), hex.EncodeToString(h.CompressedStreamDigest)))
	}

	fmt.Fprintln(w, "\nContents")
	var outputPos uint64
	for _, r := range info.Refs {
		if gap := r.Offset - outputPos; gap > 0 {
			fmt.Fprintf(w, "  %d bytes of stream data\n", gap)
		}
		fmt.Fprintf(w, "  cas reference %s:%s %d bytes\n", hashAlgoName(h.HashAlgo), hex.EncodeToString(r.Digest), r.Size)
		outputPos = r.Offset + r.Size
	}
	if total := info.ReconstructedSize(); outputPos < total {
		fmt.Fprintf(w, "  %d bytes of stream data\n", total-outputPos)
	}

	reconstructed := info.ReconstructedSize()
	fmt.Fprintln(w, "\nStatistics")
	row(w, "CAS references:", fmt.Sprintf("%d", len(info.Refs)))
	row(w, "Referenced content (in CAS):", humanizeBytes(info.ReferencedBytes()))
	row(w, "Byte stream (uncompressed):", humanizeBytes(info.StreamUncompressedSize))
	row(w, fmt.Sprintf("Byte stream (on disk, %s):", streamCompressionName(h.StreamCompression)), humanizeBytes(h.StreamSize))
	row(w, "Reconstructed tar (uncompressed):", humanizeBytes(reconstructed))
	layerLabel := fmt.Sprintf("Reconstructed layer (%s):", originalCompressionName(h.OriginalCompression.Compression))
	if h.HasCompressedStreamInfo {
		row(w, layerLabel, humanizeBytes(h.CompressedStreamSize))
	} else {
		row(w, layerLabel, "unknown (not recorded in the compact stream)")
	}
	row(w, "Compact stream file size:", humanizeBytes(uint64(indexSize)))

	fmt.Fprintln(w, "\nEfficiency")
	writeEfficiency(w, "Compact stream vs reconstructed tar (uncompressed):", uint64(indexSize), reconstructed)
	if h.HasCompressedStreamInfo && h.CompressedStreamSize > 0 {
		writeEfficiency(w, "Compact stream vs reconstructed layer (compressed):", uint64(indexSize), h.CompressedStreamSize)
	}
}

// row prints a label/value pair with the label left-padded to a fixed width.
func row(w io.Writer, label, value string) {
	fmt.Fprintf(w, "  %-34s %s\n", label, value)
}

// writeEfficiency reports part as a percentage of whole, plus an "Nx smaller"
// factor when part is non-zero.
func writeEfficiency(w io.Writer, label string, part, whole uint64) {
	if whole == 0 {
		row(w, label, "n/a")
		return
	}
	pct := float64(part) / float64(whole) * 100
	value := fmt.Sprintf("%.2f%%", pct)
	if part > 0 {
		value += fmt.Sprintf(" (%.1fx smaller)", float64(whole)/float64(part))
	}
	row(w, label, value)
}

func hashAlgoName(algo uint16) string {
	switch algo {
	case compactstream.HashAlgoSHA256:
		return "sha256"
	default:
		return fmt.Sprintf("unknown(%d)", algo)
	}
}

func streamCompressionName(c uint8) string {
	switch c {
	case compactstream.StreamCompressionNone:
		return "none"
	case compactstream.StreamCompressionZstd:
		return "zstd"
	default:
		return fmt.Sprintf("unknown(%d)", c)
	}
}

func originalCompressionName(c uint8) string {
	switch c {
	case compactstream.OriginalCompressionNone:
		return "none"
	case compactstream.OriginalCompressionGzip:
		return "gzip"
	case compactstream.OriginalCompressionZstd:
		return "zstd"
	default:
		return fmt.Sprintf("unknown(%d)", c)
	}
}

func describeOriginalCompression(o compactstream.OriginalCompressionInfo) string {
	name := originalCompressionName(o.Compression)
	if o.Compression == compactstream.OriginalCompressionNone {
		return name
	}
	level := "default"
	if o.CompressionLevel >= 0 {
		level = fmt.Sprintf("%d", o.CompressionLevel)
	}
	jobs := "default"
	if o.CompressorJobs > 0 {
		jobs = fmt.Sprintf("%d", o.CompressorJobs)
	}
	return fmt.Sprintf("%s (level %s, jobs %s)", name, level, jobs)
}

// humanizeBytes renders a byte count as raw bytes plus a binary-unit
// approximation once it reaches 1 KiB.
func humanizeBytes(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d bytes", n)
	}
	const unit = 1024.0
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(n) / unit
	i := 0
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	return fmt.Sprintf("%d bytes (%.1f %s)", n, value, units[i])
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
