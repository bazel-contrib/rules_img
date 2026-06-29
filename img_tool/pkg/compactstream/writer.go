package compactstream

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

const (
	magic = "CASSTR"

	// FormatVersion is the on-disk format version written to and validated in
	// the header.
	FormatVersion uint8 = 0x01

	headerSize = 128

	// flagCompressedStreamInfo marks (in the header Flags byte) that the
	// optional compressed-stream digest and size fields are present and valid.
	flagCompressedStreamInfo uint8 = 0x01

	HashAlgoSHA256 uint16 = 1

	StreamCompressionNone uint8 = 0
	StreamCompressionZstd uint8 = 1

	OriginalCompressionNone uint8 = 0
	OriginalCompressionGzip uint8 = 1
	OriginalCompressionZstd uint8 = 2
)

type OriginalCompressionInfo struct {
	Compression      uint8
	Seekable         bool
	CompressionLevel int8
	CompressorJobs   uint8
	EndPadding       uint32
}

type casRef struct {
	offset uint64
	digest []byte
	size   uint64
}

type Writer struct {
	output              io.Writer
	streamBuf           bytes.Buffer
	refs                []casRef
	hashAlgo            uint16
	hashSize            int
	streamCompression   uint8
	originalCompression OriginalCompressionInfo
	currentOffset       uint64
	inlineThreshold     int64
	// compressedStreamDigest and compressedStreamSize optionally record the
	// digest and size of the reconstructed, compressed stream (the original
	// compressed file). They are set via SetCompressedStreamInfo and emitted in
	// the header only when hasCompressedStreamInfo is true.
	compressedStreamDigest  []byte
	compressedStreamSize    uint64
	hasCompressedStreamInfo bool
	err                     error
}

func NewWriter(w io.Writer, hashAlgo uint16, hashSize uint16,
	streamCompression uint8, originalCompression OriginalCompressionInfo,
	inlineThreshold int64) *Writer {
	return &Writer{
		output:              w,
		hashAlgo:            hashAlgo,
		hashSize:            int(hashSize),
		streamCompression:   streamCompression,
		originalCompression: originalCompression,
		inlineThreshold:     inlineThreshold,
	}
}

func (w *Writer) InlineThreshold() int64 {
	return w.inlineThreshold
}

// SetCompressedStreamInfo records the digest and size of the reconstructed,
// compressed stream (the original compressed file). These are optional: when
// set, they are written to the header and validated during reconstruction. The
// digest length must match the index's hash size.
//
// This information is cheap to capture when the file is produced in a single
// pass (the compressor already computes it) but unknown when an index is built
// incrementally, hence its optionality.
func (w *Writer) SetCompressedStreamInfo(digest []byte, size uint64) error {
	if w.err != nil {
		return w.err
	}
	if len(digest) != w.hashSize {
		w.err = fmt.Errorf("compact stream: compressed stream digest length %d does not match expected hash size %d", len(digest), w.hashSize)
		return w.err
	}
	d := make([]byte, len(digest))
	copy(d, digest)
	w.compressedStreamDigest = d
	w.compressedStreamSize = size
	w.hasCompressedStreamInfo = true
	return nil
}

func (w *Writer) WriteStreamBytes(data []byte) error {
	if w.err != nil {
		return w.err
	}
	if _, err := w.streamBuf.Write(data); err != nil {
		w.err = err
		return err
	}
	w.currentOffset += uint64(len(data))
	return nil
}

func (w *Writer) WriteCASRef(digest []byte, size uint64) error {
	if w.err != nil {
		return w.err
	}
	if len(digest) != w.hashSize {
		w.err = fmt.Errorf("compact stream: digest length %d does not match expected hash size %d", len(digest), w.hashSize)
		return w.err
	}
	d := make([]byte, len(digest))
	copy(d, digest)
	w.refs = append(w.refs, casRef{
		offset: w.currentOffset,
		digest: d,
		size:   size,
	})
	w.currentOffset += size
	return nil
}

func (w *Writer) Close() error {
	if w.err != nil {
		return w.err
	}

	refEntrySize := 16 + w.hashSize
	refTableSize := uint64(len(w.refs) * refEntrySize)
	refTableOffset := uint64(headerSize)

	streamOffset := refTableOffset + refTableSize

	var compressedStream bytes.Buffer
	switch w.streamCompression {
	case StreamCompressionZstd:
		enc, err := zstd.NewWriter(&compressedStream)
		if err != nil {
			w.err = fmt.Errorf("creating zstd encoder for stream: %w", err)
			return w.err
		}
		if _, err := enc.Write(w.streamBuf.Bytes()); err != nil {
			enc.Close()
			w.err = err
			return err
		}
		if err := enc.Close(); err != nil {
			w.err = err
			return err
		}
	default:
		compressedStream = w.streamBuf
	}

	streamSize := uint64(compressedStream.Len())

	var header [headerSize]byte
	copy(header[0:6], magic)
	header[6] = 0x00
	header[7] = FormatVersion
	binary.BigEndian.PutUint16(header[8:10], w.hashAlgo)
	binary.BigEndian.PutUint16(header[10:12], uint16(w.hashSize))
	header[12] = w.streamCompression
	header[13] = w.originalCompression.Compression
	if w.originalCompression.Seekable {
		header[14] = 1
	}
	header[15] = byte(w.originalCompression.CompressionLevel)
	header[16] = w.originalCompression.CompressorJobs
	// bytes 17-19: reserved
	binary.BigEndian.PutUint32(header[20:24], w.originalCompression.EndPadding)
	binary.BigEndian.PutUint64(header[24:32], refTableOffset)
	binary.BigEndian.PutUint64(header[32:40], refTableSize)
	binary.BigEndian.PutUint64(header[40:48], streamOffset)
	binary.BigEndian.PutUint64(header[48:56], streamSize)
	// byte 56: flags; bytes 57-63: reserved
	if w.hasCompressedStreamInfo {
		header[56] |= flagCompressedStreamInfo
		binary.BigEndian.PutUint64(header[64:72], w.compressedStreamSize)
		// The digest occupies HashSize bytes of the fixed 56-byte slot at
		// offset 72; the remainder stays zero. Only fixed-size hashes that fit
		// the slot are supported (SHA-256 uses 32 of 56 bytes).
		copy(header[72:72+w.hashSize], w.compressedStreamDigest)
	}
	// remaining bytes up to headerSize: reserved

	if _, err := w.output.Write(header[:]); err != nil {
		w.err = err
		return err
	}

	for _, ref := range w.refs {
		var entry [16]byte
		binary.BigEndian.PutUint64(entry[0:8], ref.offset)
		if _, err := w.output.Write(entry[0:8]); err != nil {
			w.err = err
			return err
		}
		if _, err := w.output.Write(ref.digest); err != nil {
			w.err = err
			return err
		}
		binary.BigEndian.PutUint64(entry[8:16], ref.size)
		if _, err := w.output.Write(entry[8:16]); err != nil {
			w.err = err
			return err
		}
	}

	if _, err := w.output.Write(compressedStream.Bytes()); err != nil {
		w.err = err
		return err
	}

	return nil
}

func CaptureTarHeaderBytes(hdr *tar.Header) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
