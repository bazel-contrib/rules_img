package tarcas

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

const (
	indexMagic   = "CASSTR"
	indexVersion = 0x01

	indexHeaderSize = 80

	IndexHashAlgoSHA256 uint16 = 1

	IndexStreamCompressionNone uint8 = 0
	IndexStreamCompressionZstd uint8 = 1

	IndexOriginalCompressionNone uint8 = 0
	IndexOriginalCompressionGzip uint8 = 1
	IndexOriginalCompressionZstd uint8 = 2
)

type OriginalCompressionInfo struct {
	Compression      uint8
	Seekable         bool
	CompressionLevel int8
	CompressorJobs   uint8
	EndPadding       uint32
}

type casRef struct {
	offset    uint64
	digest    []byte
	size      uint64
	localPathHint string
}

type IndexWriter struct {
	output              io.Writer
	streamBuf           bytes.Buffer
	refs                []casRef
	hashAlgo            uint16
	hashSize            int
	streamCompression   uint8
	originalCompression OriginalCompressionInfo
	currentOffset       uint64
	inlineThreshold     int64
	err                 error
}

func NewIndexWriter(w io.Writer, hashAlgo uint16, hashSize uint16,
	streamCompression uint8, originalCompression OriginalCompressionInfo,
	inlineThreshold int64) *IndexWriter {
	return &IndexWriter{
		output:              w,
		hashAlgo:            hashAlgo,
		hashSize:            int(hashSize),
		streamCompression:   streamCompression,
		originalCompression: originalCompression,
		inlineThreshold:     inlineThreshold,
	}
}

func (w *IndexWriter) InlineThreshold() int64 {
	return w.inlineThreshold
}

func (w *IndexWriter) WriteStreamBytes(data []byte) error {
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

func (w *IndexWriter) WriteCASRef(digest []byte, size uint64, localPathHint string) error {
	if w.err != nil {
		return w.err
	}
	if len(digest) != w.hashSize {
		w.err = fmt.Errorf("cas stream index: digest length %d does not match expected hash size %d", len(digest), w.hashSize)
		return w.err
	}
	d := make([]byte, len(digest))
	copy(d, digest)
	w.refs = append(w.refs, casRef{
		offset:    w.currentOffset,
		digest:    d,
		size:      size,
		localPathHint: localPathHint,
	})
	w.currentOffset += size
	return nil
}

func (w *IndexWriter) Close() error {
	if w.err != nil {
		return w.err
	}

	refEntrySize := 16 + w.hashSize
	refTableSize := uint64(len(w.refs) * refEntrySize)
	refTableOffset := uint64(indexHeaderSize)

	// Build local path table: null-terminated strings concatenated
	var localPathTable []byte
	hasLocalPaths := false
	for _, ref := range w.refs {
		if ref.localPathHint != "" {
			hasLocalPaths = true
		}
		localPathTable = append(localPathTable, ref.localPathHint...)
		localPathTable = append(localPathTable, 0)
	}
	if !hasLocalPaths {
		localPathTable = nil
	}

	var localPathTableOffset, localPathTableSize uint64
	if len(localPathTable) > 0 {
		localPathTableOffset = refTableOffset + refTableSize
		localPathTableSize = uint64(len(localPathTable))
	}

	streamOffset := refTableOffset + refTableSize + localPathTableSize

	var compressedStream bytes.Buffer
	switch w.streamCompression {
	case IndexStreamCompressionZstd:
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

	var header [indexHeaderSize]byte
	copy(header[0:6], indexMagic)
	header[6] = 0x00
	header[7] = indexVersion
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
	binary.BigEndian.PutUint64(header[56:64], localPathTableOffset)
	binary.BigEndian.PutUint64(header[64:72], localPathTableSize)
	// bytes 72-79: reserved

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

	if len(localPathTable) > 0 {
		if _, err := w.output.Write(localPathTable); err != nil {
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

func captureTarHeaderBytes(hdr *tar.Header) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
