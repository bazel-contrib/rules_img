package compress

import (
	"io"

	"go.podman.io/storage/pkg/chunked/compressor"
	"github.com/klauspost/compress/zstd"
)

type ZstdChunkedWriter struct {
	writer      io.WriteCloser
	contentHash CountingResumableHash
	metadata    map[string]string
}

func NewZstdChunkedWriter(compressed io.Writer, contentHash CountingResumableHash) *ZstdChunkedWriter {
	w, _ := NewZstdChunkedWriterLevel(compressed, contentHash, int(zstd.SpeedDefault))
	return w
}

func NewZstdChunkedWriterLevel(compressed io.Writer, contentHash CountingResumableHash, level int) (*ZstdChunkedWriter, error) {
	metadata := make(map[string]string)
	zstdLevel := encoderLevelToZstd(zstd.EncoderLevel(level))
	w, err := compressor.ZstdCompressor(compressed, metadata, &zstdLevel)
	if err != nil {
		return nil, err
	}
	return &ZstdChunkedWriter{writer: w, contentHash: contentHash, metadata: metadata}, nil
}

// Inverse of zstd.EncoderLevelFromZstd, which ZstdCompressor applies to its level.
func encoderLevelToZstd(level zstd.EncoderLevel) int {
	switch level {
	case zstd.SpeedFastest:
		return 1
	case zstd.SpeedBetterCompression:
		return 6
	case zstd.SpeedBestCompression:
		return 10
	default:
		return 3
	}
}

func (z *ZstdChunkedWriter) AppendTar(r io.Reader) error {
	_, err := io.Copy(io.MultiWriter(z.writer, z.contentHash), r)
	return err
}

func (z *ZstdChunkedWriter) Close() error {
	return z.writer.Close()
}

func (z *ZstdChunkedWriter) Annotations() map[string]string {
	return z.metadata
}

type ZstdChunkedCompressorMaker struct{}

func (ZstdChunkedCompressorMaker) NewWriter(compressed io.Writer, contentHash CountingResumableHash) *ZstdChunkedWriter {
	return NewZstdChunkedWriter(compressed, contentHash)
}

func (ZstdChunkedCompressorMaker) NewWriterLevel(compressed io.Writer, contentHash CountingResumableHash, level int) (*ZstdChunkedWriter, error) {
	return NewZstdChunkedWriterLevel(compressed, contentHash, level)
}

func (ZstdChunkedCompressorMaker) Name() string {
	return "zstd-chunked"
}
