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

// TestStreamHoldsEverythingButTheReferencedFiles is the defining property of
// the compact stream of an image: the references are exactly the files of at
// least the minimum size (a merged file once), and every other byte of the
// image is in the stream. So without any blobs, reconstruction yields the image
// with just the referenced ranges zeroed, and with the blobs it is the image.
func TestStreamHoldsEverythingButTheReferencedFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ext4.Options
		min  int64
	}{
		{"4k", ext4.Options{}, 0},
		{"1k", ext4.Options{BlockSize: 1024}, 0},
		{"dedup", ext4.Options{Deduplicate: true}, 0},
		{"threshold", ext4.Options{}, 1},
		{"large-free-space", ext4.Options{Size: 256 << 20}, 0},
		{"no-lost-found", ext4.Options{OmitLostAndFound: true}, 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.DigestFiles = true
			b, err := ext4.NewBuilder(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()

			blobs := store{}
			add := func(name string, content []byte, mutate func(*tar.Header)) {
				t.Helper()
				sum := sha256.Sum256(content)
				blobs[string(sum[:])] = content
				hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644}
				if mutate != nil {
					mutate(hdr)
				}
				if err := b.WriteRegularDeduplicated(hdr, bytes.NewReader(content)); err != nil {
					t.Fatal(err)
				}
			}
			minSize := tc.min
			if minSize == 0 {
				minSize = DefaultMinSize
			}
			// Files around the threshold, an empty one, duplicates and a hard link.
			add("below", pattern(int(minSize)-1, 1), nil)
			add("exact", pattern(int(minSize), 2), nil)
			add("above", pattern(int(minSize)+1, 3), nil)
			add("empty", nil, nil)
			add("dup/a", pattern(30000, 4), nil)
			add("dup/b", pattern(30000, 4), nil)
			add("xattr", pattern(9000, 5), func(h *tar.Header) {
				h.PAXRecords = map[string]string{"SCHILY.xattr.user.k": "v"}
			})
			add("holes", append(make([]byte, 20000), pattern(5000, 6)...), nil)
			if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "link-to-above", Linkname: "above"}); err != nil {
				t.Fatal(err)
			}
			if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "symlink", Linkname: "above"}); err != nil {
				t.Fatal(err)
			}

			img, err := os.Create(filepath.Join(t.TempDir(), "image"))
			if err != nil {
				t.Fatal(err)
			}
			defer img.Close()
			var stream bytes.Buffer
			info, err := Build(b, img, &stream, Options{MinSize: tc.min})
			if err != nil {
				t.Fatal(err)
			}
			image, err := os.ReadFile(img.Name())
			if err != nil {
				t.Fatal(err)
			}

			// The references are the qualifying files, in image order.
			cs, err := compactstream.Inspect(bytes.NewReader(stream.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			var want []ext4.FileLayout
			for _, f := range info.Files {
				if f.Size >= minSize {
					want = append(want, f)
				}
			}
			if len(cs.Refs) != len(want) || len(want) == 0 {
				t.Fatalf("%d references, want %d", len(cs.Refs), len(want))
			}
			zeroed := append([]byte(nil), image...)
			for i, ref := range cs.Refs {
				f := want[i]
				if int64(ref.Offset) != f.Offset || int64(ref.Size) != f.Size || !bytes.Equal(ref.Digest, f.Digest) {
					t.Errorf("reference %d is %+v, want file %+v", i, ref, f)
				}
				if got := sha256.Sum256(image[f.Offset : f.Offset+f.Size]); !bytes.Equal(got[:], ref.Digest) {
					t.Errorf("%s: the digest does not match the bytes in the image", f.Path)
				}
				clear(zeroed[f.Offset : f.Offset+f.Size])
			}
			if hdr := cs.Header; !hdr.HasCompressedStreamInfo || hdr.CompressedStreamSize != uint64(len(image)) {
				t.Errorf("header: %+v", hdr)
			} else if sum := sha256.Sum256(image); !bytes.Equal(hdr.CompressedStreamDigest, sum[:]) {
				t.Error("the header does not carry the digest of the image")
			}

			// Without blobs: everything but the referenced bytes.
			var skeleton bytes.Buffer
			if err := compactstream.ReconstructUncompressed(context.Background(), bytes.NewReader(stream.Bytes()), compactstream.NullBlobStore{}, &skeleton); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(skeleton.Bytes(), zeroed) {
				t.Error("the stream without blobs is not the image with the referenced files zeroed")
			}

			// With blobs: the image itself.
			var rebuilt bytes.Buffer
			if err := compactstream.Reconstruct(context.Background(), bytes.NewReader(stream.Bytes()), blobs, &rebuilt); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rebuilt.Bytes(), image) {
				t.Error("the reconstructed image differs")
			}
		})
	}
}
