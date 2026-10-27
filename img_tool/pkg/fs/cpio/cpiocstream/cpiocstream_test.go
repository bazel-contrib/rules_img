package cpiocstream

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio"
)

type store map[string][]byte

func (s store) ReaderForBlob(_ context.Context, digest []byte, size int64) (io.ReadCloser, error) {
	b, ok := s[string(digest)]
	if !ok || int64(len(b)) != size {
		return nil, fmt.Errorf("blob %x (%d bytes) not in store", digest, size)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

func buildArchive(t *testing.T) (*cpio.Info, []byte, store) {
	t.Helper()
	b, err := cpio.NewBuilder(cpio.Options{DigestFiles: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	blobs := store{}
	add := func(name string, content []byte) {
		t.Helper()
		sum := sha256.Sum256(content)
		blobs[string(sum[:])] = content
		if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o755}, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	add("small", []byte("tiny"))
	add("usr/bin/big", pattern(100_000, 1))
	add("usr/lib/odd", pattern(5001, 2)) // not a multiple of four
	add("usr/lib/zeros", make([]byte, 20_000))
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "sbin/init", Linkname: "/usr/bin/big"}); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "usr/bin/alias", Linkname: "usr/bin/big"}); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	info, err := b.Build(&archive)
	if err != nil {
		t.Fatal(err)
	}
	return info, archive.Bytes(), blobs
}

// TestReconstruct checks the defining property: the references are the files of
// at least the minimum size, and with the blobs, reconstruction yields the
// archive byte for byte, also when the archive is compressed on the way.
func TestReconstruct(t *testing.T) {
	info, archive, blobs := buildArchive(t)

	// The hard link set carries its content once.
	if len(info.Files) != 4 {
		t.Fatalf("%d files with content, want 4: %+v", len(info.Files), info.Files)
	}
	for _, f := range info.Files {
		sum := sha256.Sum256(archive[f.Offset : f.Offset+f.Size])
		if !bytes.Equal(sum[:], f.Digest) {
			t.Errorf("file at %d: digest does not match the archive", f.Offset)
		}
	}

	for _, tc := range []struct {
		name string
		alg  string
		orig uint8
		min  int64
		refs int
	}{
		{"none", "none", compactstream.OriginalCompressionNone, 0, 3},
		{"none-threshold", "none", compactstream.OriginalCompressionNone, 8192, 2},
		{"gzip", "gzip", compactstream.OriginalCompressionGzip, 0, 3},
		{"zstd", "zstd", compactstream.OriginalCompressionZstd, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The layer file, like the layer command makes it.
			var layer bytes.Buffer
			appender, err := compress.AppenderFactory("sha256", tc.alg, &layer)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := appender.Write(archive); err != nil {
				t.Fatal(err)
			}
			state, err := appender.Finalize()
			if err != nil {
				t.Fatal(err)
			}

			var stream bytes.Buffer
			err = Write(info, bytes.NewReader(archive), &stream, Options{
				MinSize:          tc.min,
				Original:         compactstream.OriginalCompressionInfo{Compression: tc.orig, CompressionLevel: -1},
				CompressedDigest: state.OuterHash,
				CompressedSize:   uint64(state.CompressedSize),
			})
			if err != nil {
				t.Fatal(err)
			}
			cs, err := compactstream.Inspect(bytes.NewReader(stream.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if len(cs.Refs) != tc.refs {
				t.Errorf("%d references, want %d", len(cs.Refs), tc.refs)
			}

			var got bytes.Buffer
			if err := compactstream.Reconstruct(context.Background(), bytes.NewReader(stream.Bytes()), blobs, &got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), layer.Bytes()) {
				t.Errorf("reconstructed %d bytes that differ from the %d byte layer file", got.Len(), layer.Len())
			}
		})
	}
}

func TestNeedsDigests(t *testing.T) {
	b, _ := cpio.NewBuilder(cpio.Options{})
	defer b.Close()
	if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 3}, bytes.NewReader([]byte("abc"))); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	info, err := b.Build(&buf)
	if err != nil {
		t.Fatal(err)
	}
	info.Files = []cpio.File{{Offset: 0, Size: 3}}
	if err := Write(info, &buf, io.Discard, Options{}); err == nil {
		t.Fatal("a missing digest was accepted")
	}
}
