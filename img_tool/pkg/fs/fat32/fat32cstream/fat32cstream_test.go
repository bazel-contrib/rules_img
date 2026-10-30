package fat32cstream

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32"
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

func newImage(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "image"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TestStreamHoldsEverythingButTheReferencedFiles is the defining property of
// the compact stream of an image: the references are exactly the files of at
// least the minimum size, and every other byte of the image is in the stream.
// So without any blobs, reconstruction yields the image with just the
// referenced ranges zeroed, and with the blobs it is the image.
func TestStreamHoldsEverythingButTheReferencedFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts fat32.Options
		min  int64
	}{
		{"default", fat32.Options{}, 0},
		{"cluster-4k", fat32.Options{ClusterSize: 4096}, 0},
		{"sector-4k", fat32.Options{SectorSize: 4096}, 0},
		{"threshold", fat32.Options{}, 1},
		{"large-free-space", fat32.Options{Size: 512 << 20}, 0},
		{"high-threshold", fat32.Options{Label: "CSTREAM"}, 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.DigestFiles, opts.DigestImage = true, true
			b, err := fat32.NewBuilder(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()

			blobs := store{}
			add := func(name string, content []byte) {
				t.Helper()
				sum := sha256.Sum256(content)
				blobs[string(sum[:])] = content
				hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644}
				if err := b.WriteRegularDeduplicated(hdr, bytes.NewReader(content)); err != nil {
					t.Fatal(err)
				}
			}
			minSize := tc.min
			if minSize == 0 {
				minSize = DefaultMinSize
			}
			// Files around the threshold, an empty one, duplicates and a hard link.
			add("below", pattern(int(minSize)-1, 1))
			add("exact", pattern(int(minSize), 2))
			add("above", pattern(int(minSize)+1, 3))
			add("empty", nil)
			add("dup/a", pattern(30000, 4))
			add("dup/B with a long name", pattern(30000, 4))
			add("holes", append(make([]byte, 20000), pattern(5000, 6)...))
			if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "link-to-above", Linkname: "above"}); err != nil {
				t.Fatal(err)
			}

			img := newImage(t)
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
			if cs.Header.ContentFormat != compactstream.ContentFormatFat32 {
				t.Errorf("content format %d, want fat32 (%d)", cs.Header.ContentFormat, compactstream.ContentFormatFat32)
			}
			if got := sha256.Sum256(image); !bytes.Equal(got[:], info.Digest) {
				t.Errorf("image digest %x, want %x", info.Digest, got)
			}
			var want []fat32.FileLayout
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

func TestBuildErrors(t *testing.T) {
	b, err := fat32.NewBuilder(fat32.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 5}, bytes.NewReader([]byte("12345"))); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(b, newImage(t), io.Discard, Options{}); err == nil {
		t.Error("builder without digests accepted")
	}
}
