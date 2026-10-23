package ext4cstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"archive/tar"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
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

func TestBuildAndReconstruct(t *testing.T) {
	files := map[string][]byte{
		"big":       pattern(2<<20+17, 1),
		"medium":    pattern(10000, 2),
		"small":     pattern(100, 3),
		"edge":      pattern(4096, 4),
		"nearedge":  pattern(4095, 5),
		"dir/inner": pattern(70000, 6),
		"zeros":     make([]byte, 50000),
	}
	blobs := store{}
	b, err := ext4.NewBuilder(ext4.Options{DigestFiles: true, Size: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for _, name := range []string{"big", "medium", "small", "edge", "nearedge", "dir/inner", "zeros"} {
		c := files[name]
		sum := sha256.Sum256(c)
		blobs[string(sum[:])] = c
		if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(c)), Mode: 0o644}, bytes.NewReader(c)); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	img, err := os.Create(filepath.Join(dir, "image"))
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	var stream bytes.Buffer
	info, err := Build(b, img, &stream, Options{})
	if err != nil {
		t.Fatal(err)
	}

	// Reconstruction yields the image bit for bit, and verifies its digest.
	want, err := os.ReadFile(img.Name())
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := compactstream.Reconstruct(context.Background(), bytes.NewReader(stream.Bytes()), blobs, &got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatal("reconstructed image differs")
	}

	// 64 MiB image: the stream is tiny, the big file is not in it.
	if stream.Len() > 20<<10 {
		t.Errorf("compact stream is %d bytes for a %d byte image", stream.Len(), info.Size)
	}

	// The stream references the files of at least 4096 bytes and no others,
	// at the places they have in the image.
	cs, err := compactstream.Inspect(bytes.NewReader(stream.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var wantRefs []ext4.FileLayout
	for _, f := range info.Files {
		if f.Size >= DefaultMinSize {
			wantRefs = append(wantRefs, f)
		}
	}
	if len(cs.Refs) != 5 || len(wantRefs) != 5 {
		t.Fatalf("%d references, want 5 (%d files qualify)", len(cs.Refs), len(wantRefs))
	}
	for i, ref := range cs.Refs {
		if int64(ref.Offset) != wantRefs[i].Offset || int64(ref.Size) != wantRefs[i].Size || !bytes.Equal(ref.Digest, wantRefs[i].Digest) {
			t.Errorf("reference %d: %+v, want file %+v", i, ref, wantRefs[i])
		}
	}
}

func TestMinSizeAndErrors(t *testing.T) {
	newBuilder := func(digest bool) *ext4.Builder {
		b, err := ext4.NewBuilder(ext4.Options{DigestFiles: digest})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { b.Close() })
		c := pattern(20000, 9)
		b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: int64(len(c))}, bytes.NewReader(c))
		return b
	}
	img := func() Image {
		f, err := os.Create(filepath.Join(t.TempDir(), "image"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}

	// A threshold above the file keeps it inline: no store needed.
	var stream bytes.Buffer
	if _, err := Build(newBuilder(true), img(), &stream, Options{MinSize: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := compactstream.Reconstruct(context.Background(), bytes.NewReader(stream.Bytes()), store{}, &got); err != nil {
		t.Fatal(err)
	}

	if _, err := Build(newBuilder(false), img(), io.Discard, Options{}); err == nil {
		t.Error("builder without digests accepted")
	}
}
