package tarcas

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
)

// The tar written for a single 5 byte file: a header block and one data block.
const (
	tarWithoutEndOfArchive = 2 * 512
	tarWithEndOfArchive    = tarWithoutEndOfArchive + 2*512
)

func decompressForTest(t *testing.T, compression string, blob []byte) []byte {
	t.Helper()
	switch compression {
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(blob))
		if err != nil {
			t.Fatal(err)
		}
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return out
	case "zstd":
		r, err := zstd.NewReader(bytes.NewReader(blob))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	return blob
}

// TestEndOfArchive checks that Close terminates the tar with two zero blocks
// exactly when EndOfArchive is set, and that a compact stream recorded alongside
// reconstructs the layer, end-of-archive blocks included, bit for bit.
func TestEndOfArchive(t *testing.T) {
	content := []byte("hello")
	for _, tc := range []struct {
		name        string
		compression string
		format      compress.TarFormat
	}{
		{"uncompressed", "none", compress.TarFormatPlain},
		{"gzip", "gzip", compress.TarFormatPlain},
		{"zstd", "zstd", compress.TarFormatPlain},
		{"zstd-chunked", "zstd", compress.TarFormatZstdChunked},
	} {
		for _, endOfArchive := range []bool{false, true} {
			name := tc.name + "/without"
			wantSize := tarWithoutEndOfArchive
			if endOfArchive {
				name = tc.name + "/with"
				wantSize = tarWithEndOfArchive
			}
			t.Run(name, func(t *testing.T) {
				settings := tarSettings{compression: tc.compression, format: tc.format, compressionLevel: 3, compressorJobs: 1}

				var blob bytes.Buffer
				appender, err := compress.TarAppenderFactory("sha256", tc.compression, tc.format, &blob, settings.compressOpts()...)
				if err != nil {
					t.Fatal(err)
				}
				var cstream bytes.Buffer
				iw := compactstream.NewWriter(&cstream, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionZstd,
					compactstream.OriginalCompressionInfo{
						Compression:      settings.originalCompression(),
						Format:           settings.format,
						CompressionLevel: int8(settings.compressionLevel),
						CompressorJobs:   uint8(settings.compressorJobs),
					}, 0)

				c := New[SHA256Helper](appender, WithCompactStreamWriter{Writer: iw}, EndOfArchive(endOfArchive))
				if err := c.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "file.txt", Size: int64(len(content)), Mode: 0o644}, bytes.NewReader(content)); err != nil {
					t.Fatal(err)
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				if _, err := appender.Finalize(); err != nil {
					t.Fatal(err)
				}
				if err := iw.Close(); err != nil {
					t.Fatal(err)
				}

				raw := decompressForTest(t, tc.compression, blob.Bytes())
				if len(raw) != wantSize {
					t.Fatalf("tar is %d bytes, want %d", len(raw), wantSize)
				}
				if endOfArchive && !bytes.Equal(raw[tarWithoutEndOfArchive:], make([]byte, 2*512)) {
					t.Fatalf("end-of-archive blocks are not zero: %x", raw[tarWithoutEndOfArchive:])
				}
				tr := tar.NewReader(bytes.NewReader(raw))
				if _, err := tr.Next(); err != nil {
					t.Fatal(err)
				}
				if _, err := tr.Next(); err != io.EOF {
					t.Fatalf("second Next() = %v, want io.EOF", err)
				}

				store := newMemBlobStore()
				store.Store(content)
				var reconstructed bytes.Buffer
				if err := compactstream.Reconstruct(context.Background(), &cstream, store, &reconstructed); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(blob.Bytes(), reconstructed.Bytes()) {
					t.Fatalf("reconstruction mismatch: built=%d bytes, reconstructed=%d bytes", blob.Len(), reconstructed.Len())
				}
			})
		}
	}
}
