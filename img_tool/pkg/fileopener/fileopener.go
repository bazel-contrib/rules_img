package fileopener

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

func CompressionReaderWithFormat(r io.Reader, format api.CompressionAlgorithm) (io.Reader, error) {
	switch format {
	case api.Gzip:
		return gzip.NewReader(r)
	case api.Zstd:
		decoder, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return decoder.IOReadCloser(), nil
	case api.Uncompressed, "tar", "none":
		return r, nil
	default:
		return nil, fmt.Errorf("unsupported compression format: %s", format)
	}
}

func CompressionReader(r underlyingReader) (io.Reader, error) {
	compressionFormat, err := LearnCompressionAlgorithm(r)
	if err != nil {
		return nil, err
	}

	return CompressionReaderWithFormat(r, compressionFormat)
}

func LearnCompressionAlgorithm(r io.ReaderAt) (api.CompressionAlgorithm, error) {
	var startMagic [4]byte
	if _, err := r.ReadAt(startMagic[:], 0); err != nil {
		return "", err
	}
	if bytes.Equal(startMagic[:2], gzipMagic[:]) {
		return api.Gzip, nil
	}
	if bytes.Equal(startMagic[:4], zstdMagic[:]) {
		return api.Zstd, nil
	}
	return api.Uncompressed, nil
}

func LearnLayerFormat(r io.ReaderAt) (api.LayerFormat, error) {
	compressionFormat, err := LearnCompressionAlgorithm(r)
	if err != nil {
		return "", err
	}

	if compressionFormat != api.Uncompressed {
		switch compressionFormat {
		case api.Gzip:
			return api.TarGzipLayer, nil
		case api.Zstd:
			return api.TarZstdLayer, nil
		default:
			return "", fmt.Errorf("unsupported compression format: %s", compressionFormat)
		}
	}

	var tarMagic [8]byte
	if _, err := r.ReadAt(tarMagic[:], 257); err != nil {
		return "", err
	}
	if bytes.Equal(tarMagic[:], tarMagicA[:]) || bytes.Equal(tarMagic[:], tarMagicB[:]) {
		return api.TarLayer, nil
	}
	// Alternatively, check for end-of-archive marker of an empty tar file
	var emptyTar [1024]byte
	n, err := r.ReadAt(emptyTar[:], 0)
	if err != nil {
		return "", err
	}
	if n == 1024 && bytes.Equal(emptyTar[:], make([]byte, 1024)) {
		return api.TarLayer, nil
	}
	return "", fmt.Errorf("unknown file type")
}

type underlyingReader interface {
	io.Reader
	io.ReaderAt
}

var (
	gzipMagic = [2]byte{0x1f, 0x8b}
	zstdMagic = [4]byte{0x28, 0xb5, 0x2f, 0xfd}
	tarMagicA = [8]byte{0x75, 0x73, 0x74, 0x61, 0x72, 0x00, 0x30, 0x30}
	tarMagicB = [8]byte{0x75, 0x73, 0x74, 0x61, 0x72, 0x20, 0x20, 0x00}
)
