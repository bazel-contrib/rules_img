// Package ztoc implements pure-Go generation of a "ztoc" (zTOC), the seekable
// table-of-contents metadata used by the SOCI (Seekable OCI) snapshotter to
// lazily pull container image layers without unpacking the whole blob.
//
// A ztoc has two parts:
//
//   - a TOC (table of contents): one entry per file in the (uncompressed) tar
//     archive, recording the file's metadata and where its uncompressed bytes
//     live in the decompressed stream; and
//   - a "zinfo" ([CompressionInfo]): a set of checkpoints capturing the state of
//     the DEFLATE decompressor at various points in the compressed blob, so that
//     an arbitrary byte range can be decompressed by resuming from the nearest
//     checkpoint instead of from the start.
//
// The serialized form is a FlatBuffer whose schema is defined by upstream
// soci-snapshotter (https://github.com/awslabs/soci-snapshotter). This package
// produces byte-compatible output: ztocs built here are intended to be consumed
// unmodified by soci-snapshotter.
//
// Unlike soci-snapshotter's own builder, which shells out to a C port of zlib's
// zran.c via cgo, this implementation is pure Go: the DEFLATE decompressor in
// inflate.go is a port of Mark Adler's puff.c extended to record checkpoints,
// and no cgo or external decompression library is used.
//
// This is currently a freestanding library; only gzip-compressed (and
// uncompressed) tar layers are supported, matching soci's ztoc v0.9 / zinfo v2
// format.
package ztoc

import (
	"time"

	"github.com/opencontainers/go-digest"
)

// Offset holds a size or an offset (in either the compressed or uncompressed
// stream). It mirrors soci's compression.Offset.
type Offset int64

// SpanID identifies a span (the region of the compressed stream between two
// consecutive checkpoints). It mirrors soci's compression.SpanID.
type SpanID int32

// Version is a ztoc format version string of the form "<major>.<minor>".
type Version string

// Ztoc versions produced/understood by this package.
const (
	// Version09 is the only ztoc version soci-snapshotter currently emits.
	Version09 Version = "0.9"
)

// Compression algorithm identifiers, matching soci/containerd. Only Gzip is
// implemented for zinfo generation.
const (
	CompressionGzip         = "gzip"
	CompressionUncompressed = "uncompressed"
)

// DefaultSpanSize is the minimum number of uncompressed bytes between two
// checkpoints. It matches soci-snapshotter's default of 4 MiB.
const DefaultSpanSize int64 = 1 << 22

// DefaultBuildToolIdentifier is recorded in the ztoc's build_tool_identifier
// field when the caller does not override it.
const DefaultBuildToolIdentifier = "rules_img ztoc (pure Go)"

// Ztoc is the in-memory representation of a ztoc. Its layout mirrors soci's
// ztoc.Ztoc so the two can be marshaled to an identical FlatBuffer.
type Ztoc struct {
	TOC
	CompressionInfo

	Version                 Version
	BuildToolIdentifier     string
	CompressedArchiveSize   Offset
	UncompressedArchiveSize Offset
}

// TOC is the table-of-contents part of a ztoc: metadata for every entry in the
// uncompressed tar archive, in archive order.
type TOC struct {
	FileMetadata []FileMetadata
}

// CompressionInfo is the "zinfo" part of a ztoc: the serialized checkpoints plus
// per-span digests that let a consumer verify and seek within the compressed
// blob.
type CompressionInfo struct {
	// MaxSpanID is the ID of the last span, i.e. (number of checkpoints - 1).
	MaxSpanID SpanID
	// SpanDigests holds the sha256 digest of each span's compressed bytes.
	SpanDigests []digest.Digest
	// Checkpoints is the serialized zinfo blob (see zinfo.go for the layout).
	Checkpoints []byte
	// CompressionAlgorithm is the layer's compression algorithm ("gzip").
	CompressionAlgorithm string
}

// FileMetadata describes a single entry in the uncompressed tar archive. The
// fields mirror soci's ztoc.FileMetadata.
type FileMetadata struct {
	Name               string
	Type               string
	UncompressedOffset Offset
	UncompressedSize   Offset
	// TarHeaderOffset is the offset of this entry's tar header in the
	// uncompressed stream. It is derived on unmarshal and is not serialized.
	TarHeaderOffset Offset
	Linkname        string // target of a hardlink ("hardlink") or symlink ("symlink")
	Mode            int64  // tar header mode bits
	UID             int
	GID             int
	Uname           string
	Gname           string
	ModTime         time.Time
	Devmajor        int64 // valid for "char"/"block" entries
	Devminor        int64 // valid for "char"/"block" entries
	// PAXHeaders holds the raw PAX records from the tar header. Note the
	// FlatBuffer field is (misleadingly) named "xattrs"; it stores all PAX
	// records, not only xattrs.
	PAXHeaders map[string]string
}
