// Package extentstream implements the extent stream, an EXPERIMENTAL format
// that describes a byte sequence (a disk image, a file system image, an archive)
// as an ordered list of extents:
//
//   - data embedded in the stream,
//   - holes (zeros that are never stored),
//   - ranges of blobs in a content-addressed store (CAS),
//   - ranges of the content of a compact stream (see package compactstream),
//   - ranges of the content of another extent stream.
//
// References to other streams are by digest of the stream file, so a stream
// nests: a disk image can reference the compact stream of its FAT32 partition,
// which references the files in it. The extents tile the stream exactly: they
// start at offset zero, follow each other without gaps and add up to the size.
//
// The format is experimental: it can change incompatibly, and the version is
// not stable. See docs/extent-stream.md for the byte-level specification.
package extentstream

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Experimental is true while the format may change incompatibly.
const Experimental = true

// ExperimentalNotice is printed by tools that read or write an extent stream.
const ExperimentalNotice = "warning: the extent stream format is experimental and may change incompatibly"

const (
	// Magic starts every extent stream.
	Magic = "CASEXT"
	// FormatVersion is the version of the format written by this package.
	FormatVersion = 1
	// HeaderSize is the size of the fixed header in bytes.
	HeaderSize = 128
	// ExtentSize is the size of an encoded extent in bytes.
	ExtentSize = 64
	// HashSize is the size of the digests (SHA-256).
	HashSize = 32

	hashAlgoSHA256 = 1

	flagContentDigest = 1 << 0 // the content digest in the header is valid
	flagExperimental  = 1 << 1 // always set by this version

	dataCompressionNone = 0
	dataCompressionZstd = 1

	// maxTableSize bounds the memory needed for the extent table.
	maxTableSize = 1 << 30
	// maxChildSize bounds a referenced stream that is read into memory.
	maxChildSize = 1 << 30
	// maxDepth bounds the nesting of streams that reference streams.
	maxDepth = 16
)

// Kind is the kind of an extent.
type Kind uint8

const (
	// KindData is data embedded in the stream.
	KindData Kind = 1
	// KindHole is a region of zeros.
	KindHole Kind = 2
	// KindCAS is a range of a blob in the content-addressed store.
	KindCAS Kind = 3
	// KindCompactStream is a range of the reconstructed content of a compact stream.
	KindCompactStream Kind = 4
	// KindExtentStream is a range of the reconstructed content of an extent stream.
	KindExtentStream Kind = 5
)

func (k Kind) String() string {
	switch k {
	case KindData:
		return "data"
	case KindHole:
		return "hole"
	case KindCAS:
		return "cas"
	case KindCompactStream:
		return "compact-stream"
	case KindExtentStream:
		return "extent-stream"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// IsRef reports whether the extent points at a blob in the store.
func (k Kind) IsRef() bool {
	return k == KindCAS || k == KindCompactStream || k == KindExtentStream
}

// ContentFormat tells what the content of a stream is. Reconstruction ignores it.
type ContentFormat uint8

const (
	ContentFormatUnknown ContentFormat = 0
	ContentFormatDisk    ContentFormat = 1 // a disk image with a GPT or MBR
)

func (c ContentFormat) String() string {
	switch c {
	case ContentFormatUnknown:
		return "unknown"
	case ContentFormatDisk:
		return "disk"
	}
	return fmt.Sprintf("format(%d)", uint8(c))
}

// Extent is one entry of the extent table. Its position in the stream follows
// from the lengths of the extents before it.
type Extent struct {
	Kind   Kind
	Length uint64
	// SrcOffset is, depending on the kind:
	//   data: the offset in the (uncompressed) data section,
	//   cas: the offset in the blob,
	//   compact stream, extent stream: the offset in the reconstructed content.
	SrcOffset uint64
	// BlobSize is the size of the blob (the file of the stream) of a reference.
	BlobSize uint64
	// Digest is the SHA-256 of the blob of a reference.
	Digest [HashSize]byte
}

// Header is the fixed-size header of a stream.
type Header struct {
	Version       uint8
	ContentFormat ContentFormat
	// Size is the size of the content the stream describes.
	Size        uint64
	ExtentCount uint64
	// HasContentDigest tells whether ContentDigest is the SHA-256 of the content.
	HasContentDigest bool
	ContentDigest    [HashSize]byte

	dataCompression uint8
	tableOffset     uint64
	tableSize       uint64
	dataOffset      uint64
	dataSize        uint64 // on disk
	dataRawSize     uint64 // uncompressed
}

var errNotExtentStream = errors.New("not an extent stream")

// HasMagic reports whether the bytes start like an extent stream.
func HasMagic(prefix []byte) bool {
	return len(prefix) >= len(Magic) && string(prefix[:len(Magic)]) == Magic
}

func (h *Header) marshal() []byte {
	b := make([]byte, HeaderSize)
	copy(b, Magic)
	b[7] = h.Version
	binary.BigEndian.PutUint16(b[8:], hashAlgoSHA256)
	binary.BigEndian.PutUint16(b[10:], HashSize)
	b[12] = h.dataCompression
	flags := byte(flagExperimental)
	if h.HasContentDigest {
		flags |= flagContentDigest
	}
	b[13] = flags
	b[14] = byte(h.ContentFormat)
	binary.BigEndian.PutUint64(b[16:], h.Size)
	binary.BigEndian.PutUint64(b[24:], h.ExtentCount)
	binary.BigEndian.PutUint64(b[32:], h.tableOffset)
	binary.BigEndian.PutUint64(b[40:], h.tableSize)
	binary.BigEndian.PutUint64(b[48:], h.dataOffset)
	binary.BigEndian.PutUint64(b[56:], h.dataSize)
	binary.BigEndian.PutUint64(b[64:], h.dataRawSize)
	copy(b[72:], h.ContentDigest[:])
	return b
}

func parseHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < HeaderSize || !HasMagic(b) || b[6] != 0 {
		return h, errNotExtentStream
	}
	h.Version = b[7]
	if h.Version != FormatVersion {
		return h, fmt.Errorf("unsupported extent stream version %d (this tool reads version %d; the format is experimental)", h.Version, FormatVersion)
	}
	if algo := binary.BigEndian.Uint16(b[8:]); algo != hashAlgoSHA256 {
		return h, fmt.Errorf("unsupported hash algorithm %d", algo)
	}
	if size := binary.BigEndian.Uint16(b[10:]); size != HashSize {
		return h, fmt.Errorf("unsupported hash size %d", size)
	}
	h.dataCompression = b[12]
	if h.dataCompression != dataCompressionNone && h.dataCompression != dataCompressionZstd {
		return h, fmt.Errorf("unsupported data compression %d", h.dataCompression)
	}
	h.HasContentDigest = b[13]&flagContentDigest != 0
	h.ContentFormat = ContentFormat(b[14])
	h.Size = binary.BigEndian.Uint64(b[16:])
	h.ExtentCount = binary.BigEndian.Uint64(b[24:])
	h.tableOffset = binary.BigEndian.Uint64(b[32:])
	h.tableSize = binary.BigEndian.Uint64(b[40:])
	h.dataOffset = binary.BigEndian.Uint64(b[48:])
	h.dataSize = binary.BigEndian.Uint64(b[56:])
	h.dataRawSize = binary.BigEndian.Uint64(b[64:])
	copy(h.ContentDigest[:], b[72:])
	if h.Size > 1<<62 {
		return h, fmt.Errorf("size %d is too large", h.Size)
	}
	// The sections follow each other, which is how they are read.
	if h.tableOffset != HeaderSize || h.tableSize != h.ExtentCount*ExtentSize || h.ExtentCount > maxTableSize/ExtentSize ||
		h.dataOffset != h.tableOffset+h.tableSize {
		return h, errors.New("corrupt extent stream: inconsistent section offsets")
	}
	if h.dataCompression == dataCompressionNone && h.dataSize != h.dataRawSize {
		return h, errors.New("corrupt extent stream: uncompressed data section with different sizes")
	}
	return h, nil
}

func (e *Extent) marshal(b []byte) {
	b[0] = byte(e.Kind)
	binary.BigEndian.PutUint64(b[8:], e.Length)
	binary.BigEndian.PutUint64(b[16:], e.SrcOffset)
	binary.BigEndian.PutUint64(b[24:], e.BlobSize)
	copy(b[32:], e.Digest[:])
}

func parseExtent(b []byte) Extent {
	e := Extent{
		Kind:      Kind(b[0]),
		Length:    binary.BigEndian.Uint64(b[8:]),
		SrcOffset: binary.BigEndian.Uint64(b[16:]),
		BlobSize:  binary.BigEndian.Uint64(b[24:]),
	}
	copy(e.Digest[:], b[32:])
	return e
}
