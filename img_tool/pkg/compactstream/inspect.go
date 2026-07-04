package compactstream

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/klauspost/compress/zstd"
)

// maxRefTableSize bounds the CAS reference table size accepted from a header
// before any allocation, so a malformed or hostile compact stream cannot force
// an unbounded (multi-exabyte) allocation or a makeslice panic. The bound is
// deliberately far above any plausible layer: at 48 bytes per SHA-256 entry it
// allows ~44 million references (i.e. ~44 million distinct files in a single
// layer), which we never expect to reach in practice.
const maxRefTableSize uint64 = 1 << 31 // 2 GiB

// Header holds the parsed fields of a compact stream header. See
// docs/compact-stream.md for the on-disk layout.
type Header struct {
	Version             uint8
	HashAlgo            uint16
	HashSize            int
	StreamCompression   uint8
	OriginalCompression OriginalCompressionInfo
	RefTableOffset      uint64
	RefTableSize        uint64
	StreamOffset        uint64
	StreamSize          uint64
	// HasCompressedStreamInfo reports whether the optional compressed-stream
	// digest and size fields below are present in the header.
	HasCompressedStreamInfo bool
	// CompressedStreamDigest is the digest of the reconstructed, compressed
	// stream (the original compressed file). Valid only when
	// HasCompressedStreamInfo is true.
	CompressedStreamDigest []byte
	// CompressedStreamSize is the size in bytes of the reconstructed, compressed
	// stream. Valid only when HasCompressedStreamInfo is true.
	CompressedStreamSize uint64
}

// RefEntrySize is the size in bytes of a single CAS reference table entry
// (an 8-byte offset, the digest, and an 8-byte size).
func (h Header) RefEntrySize() int { return 16 + h.HashSize }

// RefCount is the number of CAS reference entries in the reference table.
func (h Header) RefCount() int {
	entry := h.RefEntrySize()
	if entry <= 0 {
		return 0
	}
	return int(h.RefTableSize / uint64(entry))
}

// CASReference describes one contiguous range of the reconstructed stream that
// is stored as a content-addressed blob rather than inline in the byte stream.
type CASReference struct {
	Offset uint64 // byte offset of the range in the reconstructed (uncompressed) stream
	Digest []byte // content digest of the referenced blob
	Size   uint64 // number of bytes the blob contributes to the stream
}

// Info is a structural view of a compact stream sufficient to describe
// and measure it without fetching any CAS blobs (i.e. without reconstruction).
type Info struct {
	Header Header
	Refs   []CASReference
	// StreamUncompressedSize is the size of the byte stream section after
	// decompression: the parts of the reconstructed stream that are NOT replaced
	// by CAS references (tar headers, any inlined small files, and tar block
	// padding).
	StreamUncompressedSize uint64
}

// ReferencedBytes is the total number of bytes stored as CAS references, i.e.
// the content held in the content-addressed store rather than in the index.
func (i *Info) ReferencedBytes() uint64 {
	var total uint64
	for _, r := range i.Refs {
		total += r.Size
	}
	return total
}

// ReconstructedSize is the size of the reconstructed, uncompressed stream (the
// original tar): the inline byte-stream bytes plus all CAS-referenced bytes.
func (i *Info) ReconstructedSize() uint64 {
	return i.StreamUncompressedSize + i.ReferencedBytes()
}

// ReadHeader reads and validates the fixed-size compact stream header from
// r. It consumes exactly headerSize bytes, leaving r positioned at the start
// of the CAS reference table.
func ReadHeader(r io.Reader) (Header, error) {
	var raw [headerSize]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return Header{}, fmt.Errorf("reading compact stream header: %w", err)
	}
	return parseHeader(raw)
}

func parseHeader(raw [headerSize]byte) (Header, error) {
	if string(raw[0:6]) != magic {
		return Header{}, fmt.Errorf("invalid compact stream magic: %q", raw[0:6])
	}
	if raw[6] != 0x00 {
		return Header{}, fmt.Errorf("expected NUL byte at offset 6, got %x", raw[6])
	}
	if raw[7] != FormatVersion {
		return Header{}, fmt.Errorf("unsupported compact stream version: %d", raw[7])
	}

	h := Header{
		Version:           raw[7],
		HashAlgo:          binary.BigEndian.Uint16(raw[8:10]),
		HashSize:          int(binary.BigEndian.Uint16(raw[10:12])),
		StreamCompression: raw[12],
		OriginalCompression: OriginalCompressionInfo{
			Compression:      raw[13],
			Seekable:         raw[14] == 1,
			CompressionLevel: int8(raw[15]),
			CompressorJobs:   raw[16],
			EndPadding:       binary.BigEndian.Uint32(raw[20:24]),
		},
		RefTableOffset: binary.BigEndian.Uint64(raw[24:32]),
		RefTableSize:   binary.BigEndian.Uint64(raw[32:40]),
		StreamOffset:   binary.BigEndian.Uint64(raw[40:48]),
		StreamSize:     binary.BigEndian.Uint64(raw[48:56]),
	}

	if h.HashAlgo != HashAlgoSHA256 {
		return Header{}, fmt.Errorf("unsupported hash algorithm: %d", h.HashAlgo)
	}
	// SHA-256 implies a fixed 32-byte digest. Reject a HashSize that disagrees
	// with the declared algorithm up front, so the ref-table math below and the
	// reconstruction digest check operate on a consistent digest length rather
	// than failing later with a confusing error.
	if h.HashSize != sha256.Size {
		return Header{}, fmt.Errorf("hash size %d does not match SHA-256 (expected %d)", h.HashSize, sha256.Size)
	}
	if h.RefTableSize > maxRefTableSize {
		return Header{}, fmt.Errorf("ref table size %d exceeds maximum %d", h.RefTableSize, maxRefTableSize)
	}
	if h.RefTableSize%uint64(h.RefEntrySize()) != 0 {
		return Header{}, fmt.Errorf("ref table size %d is not a multiple of entry size %d", h.RefTableSize, h.RefEntrySize())
	}

	// Optional compressed-stream info (header Flags byte at offset 56).
	if raw[56]&flagCompressedStreamInfo != 0 {
		if 72+h.HashSize > headerSize {
			return Header{}, fmt.Errorf("hash size %d too large for compressed-stream digest slot", h.HashSize)
		}
		h.HasCompressedStreamInfo = true
		h.CompressedStreamSize = binary.BigEndian.Uint64(raw[64:72])
		digest := make([]byte, h.HashSize)
		copy(digest, raw[72:72+h.HashSize])
		h.CompressedStreamDigest = digest
	}
	return h, nil
}

// readRefTable parses the CAS reference table that immediately follows the header.
// r must be positioned at the start of the table (as left by ReadHeader).
func readRefTable(r io.Reader, header Header) ([]CASReference, error) {
	refTableData := make([]byte, header.RefTableSize)
	if _, err := io.ReadFull(r, refTableData); err != nil {
		return nil, fmt.Errorf("reading CAS reference table: %w", err)
	}

	entry := header.RefEntrySize()
	refs := make([]CASReference, header.RefCount())
	var prevEnd uint64
	for i := range refs {
		base := i * entry
		digest := make([]byte, header.HashSize)
		copy(digest, refTableData[base+8:base+8+header.HashSize])
		ref := CASReference{
			Offset: binary.BigEndian.Uint64(refTableData[base : base+8]),
			Digest: digest,
			Size:   binary.BigEndian.Uint64(refTableData[base+8+header.HashSize : base+entry]),
		}

		// Validate the invariants the format promises (see docs/compact-stream.md):
		// entries are sorted by offset ascending and non-overlapping. Enforcing
		// this here turns malformed input into a clear error instead of a uint64
		// underflow (gap = offset - outputPos) that would silently corrupt the
		// reconstruction. We also bound offset and size to int64 so the io.CopyN
		// calls during reconstruction can never receive a negative count.
		if ref.Size > math.MaxInt64 {
			return nil, fmt.Errorf("CAS ref %d size %d exceeds maximum %d", i, ref.Size, int64(math.MaxInt64))
		}
		if ref.Offset > math.MaxInt64 {
			return nil, fmt.Errorf("CAS ref %d offset %d exceeds maximum %d", i, ref.Offset, int64(math.MaxInt64))
		}
		if ref.Offset < prevEnd {
			return nil, fmt.Errorf("CAS ref table not sorted/non-overlapping: ref %d offset %d precedes end of previous ref %d", i, ref.Offset, prevEnd)
		}
		prevEnd = ref.Offset + ref.Size

		refs[i] = ref
	}
	return refs, nil
}

// Inspect reads a compact stream in full and returns a structural view of
// it (header, CAS references, and the decompressed byte-stream length) without
// fetching any blobs. The byte stream is decompressed only to measure its size;
// its contents are discarded.
func Inspect(r io.Reader) (*Info, error) {
	header, err := ReadHeader(r)
	if err != nil {
		return nil, err
	}

	refs, err := readRefTable(r, header)
	if err != nil {
		return nil, err
	}

	streamLen, err := countStreamBytes(r, header.StreamCompression)
	if err != nil {
		return nil, err
	}

	return &Info{
		Header:                 header,
		Refs:                   refs,
		StreamUncompressedSize: streamLen,
	}, nil
}

// countStreamBytes consumes the byte stream section from r (positioned at the
// start of the stream) and returns its decompressed length.
func countStreamBytes(r io.Reader, streamCompression uint8) (uint64, error) {
	stream, err := streamReader(r, streamCompression)
	if err != nil {
		return 0, err
	}
	if closer, ok := stream.(io.Closer); ok {
		defer closer.Close()
	}
	n, err := io.Copy(io.Discard, stream)
	if err != nil {
		return 0, fmt.Errorf("decompressing byte stream: %w", err)
	}
	return uint64(n), nil
}

// streamReader wraps r (positioned at the byte stream section) in the decoder
// implied by streamCompression. The returned reader may implement io.Closer.
func streamReader(r io.Reader, streamCompression uint8) (io.Reader, error) {
	switch streamCompression {
	case StreamCompressionNone:
		return r, nil
	case StreamCompressionZstd:
		dec, err := zstd.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("creating zstd decoder for stream: %w", err)
		}
		return dec.IOReadCloser(), nil
	default:
		return nil, fmt.Errorf("unsupported stream compression: %d", streamCompression)
	}
}
