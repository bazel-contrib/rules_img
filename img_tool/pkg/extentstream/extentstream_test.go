package extentstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/casdirlayout"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

// memStore is a BlobStore over blobs in memory, addressed by digest.
type memStore map[[32]byte][]byte

func (m memStore) add(b []byte) ([]byte, int64) {
	sum := sha256.Sum256(b)
	m[sum] = b
	return sum[:], int64(len(b))
}

func (m memStore) ReaderForBlob(_ context.Context, digest []byte, _ int64) (io.ReadCloser, error) {
	var k [32]byte
	copy(k[:], digest)
	b, ok := m[k]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func build(t *testing.T, fn func(w *Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf)
	fn(w)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func reconstruct(t *testing.T, stream []byte, store compactstream.BlobStore) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := Reconstruct(context.Background(), bytes.NewReader(stream), store, &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestRoundTripAllKinds(t *testing.T) {
	store := memStore{}
	blob := []byte("0123456789abcdefghij")
	blobDigest, blobSize := store.add(blob)

	// A compact stream with inline bytes and a reference to the blob.
	var cs bytes.Buffer
	cw := compactstream.NewWriter(&cs, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionNone, compactstream.OriginalCompressionInfo{}, 1)
	cw.WriteStreamBytes([]byte("HEAD"))
	cw.WriteCASRef(blobDigest, uint64(blobSize))
	cw.WriteStreamBytes([]byte("TAIL"))
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	csDigest, csSize := store.add(cs.Bytes())
	csContent := "HEAD" + string(blob) + "TAIL"

	// An extent stream that is itself referenced.
	inner := build(t, func(w *Writer) {
		w.AddData([]byte("inner-"))
		w.AddHole(3)
		w.AddRef(KindCAS, blobDigest, blobSize, 10, 5)
	})
	innerDigest, innerSize := store.add(inner)
	innerContent := "inner-\x00\x00\x00" + string(blob[10:15])

	stream := build(t, func(w *Writer) {
		w.AddData([]byte("start|"))
		w.AddHole(4)
		w.AddRef(KindCAS, blobDigest, blobSize, 2, 6)
		w.AddRef(KindCompactStream, csDigest, csSize, 2, 10)
		w.AddRef(KindExtentStream, innerDigest, innerSize, 3, 9)
		w.AddData([]byte("|end"))
	})
	want := "start|" + "\x00\x00\x00\x00" + string(blob[2:8]) + csContent[2:12] + innerContent[3:12] + "|end"
	if got := reconstruct(t, stream, store); string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	idx, err := Inspect(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if idx.Header.Size != uint64(len(want)) || len(idx.Extents) != 6 {
		t.Errorf("size %d with %d extents", idx.Header.Size, len(idx.Extents))
	}
}

func TestPatchedRef(t *testing.T) {
	store := memStore{}
	blob := []byte("abcdefghijklmnopqrstuvwxyz")
	digest, size := store.add(blob)
	stream := build(t, func(w *Writer) {
		if err := w.AddRefPatched(KindCAS, digest, size, size, []Patch{{Offset: 20, Data: []byte("XY")}, {Offset: 3, Data: []byte("Z")}}); err != nil {
			t.Fatal(err)
		}
	})
	want := []byte(string(blob))
	want[3] = 'Z'
	copy(want[20:], "XY")
	if got := reconstruct(t, stream, store); !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.AddRefPatched(KindCAS, digest, size, size, []Patch{{Offset: 3, Data: []byte("ab")}, {Offset: 4, Data: []byte("c")}}); err == nil {
		t.Error("overlapping patches were accepted")
	}
}

func TestContentDigest(t *testing.T) {
	content := []byte("hello")
	sum := sha256.Sum256(append(content, make([]byte, 3)...))
	good := build(t, func(w *Writer) {
		w.AddData(content)
		w.AddHole(3)
		w.SetContentDigest(sum[:])
	})
	reconstruct(t, good, memStore{})
	bad := build(t, func(w *Writer) {
		w.AddData(content)
		w.AddHole(2)
		w.SetContentDigest(sum[:])
	})
	err := Reconstruct(context.Background(), bytes.NewReader(bad), memStore{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("a wrong content digest gave %v", err)
	}
}

func TestHolesAreSkipped(t *testing.T) {
	stream := build(t, func(w *Writer) {
		w.AddData([]byte("a"))
		w.AddHole(1 << 30)
		w.AddData([]byte("b"))
	})
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fw := &fileWriter{f: f}
	if err := Reconstruct(context.Background(), bytes.NewReader(stream), memStore{}, fw); err != nil {
		t.Fatal(err)
	}
	if fw.holes != 1<<30 {
		t.Errorf("%d bytes of holes were skipped", fw.holes)
	}
	if len(stream) > 400 {
		t.Errorf("a stream with a hole has %d bytes", len(stream))
	}
}

type fileWriter struct {
	f     *os.File
	holes int64
}

func (w *fileWriter) Write(p []byte) (int, error) { return w.f.Write(p) }
func (w *fileWriter) WriteHole(n int64) error {
	w.holes += n
	_, err := w.f.Seek(n, io.SeekCurrent)
	return err
}

func TestCorruptStreams(t *testing.T) {
	store := memStore{}
	good := build(t, func(w *Writer) {
		w.AddData([]byte("abc"))
		w.AddHole(5)
	})
	mutate := func(name string, f func(b []byte), wantErr string) {
		t.Run(name, func(t *testing.T) {
			b := append([]byte(nil), good...)
			f(b)
			err := Reconstruct(context.Background(), bytes.NewReader(b), store, io.Discard)
			if err == nil || !strings.Contains(err.Error(), wantErr) {
				t.Errorf("got %v, want an error with %q", err, wantErr)
			}
		})
	}
	mutate("magic", func(b []byte) { b[0] = 'X' }, "not an extent stream")
	mutate("version", func(b []byte) { b[7] = 9 }, "unsupported extent stream version")
	mutate("size", func(b []byte) { b[23] = 99 }, "add up")
	mutate("kind", func(b []byte) { b[HeaderSize] = 77 }, "unknown kind")
	mutate("length", func(b []byte) { b[HeaderSize+15] = 0 }, "has length")
	if err := Reconstruct(context.Background(), bytes.NewReader(good[:HeaderSize+10]), store, io.Discard); err == nil {
		t.Error("a truncated table was accepted")
	}
	if err := Reconstruct(context.Background(), bytes.NewReader(good[:len(good)-1]), store, io.Discard); err == nil {
		t.Error("a truncated data section was accepted")
	}
}

func TestReferenceErrors(t *testing.T) {
	store := memStore{}
	blob := []byte("0123456789")
	digest, size := store.add(blob)

	// A range beyond the blob is rejected when the stream is read.
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.AddRef(KindCAS, digest, size, 8, 5); err == nil {
		t.Error("the writer accepted a range beyond the blob")
	}

	missing := sha256.Sum256([]byte("missing"))
	stream := build(t, func(w *Writer) { w.AddRef(KindCAS, missing[:], 4, 0, 4) })
	if err := Reconstruct(context.Background(), bytes.NewReader(stream), store, io.Discard); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing blob gave %v", err)
	}

	// A child stream whose bytes do not match its digest.
	childDigest, childSize := store.add(build(t, func(w *Writer) { w.AddData([]byte("child")) }))
	var k [32]byte
	copy(k[:], childDigest)
	store[k] = append([]byte(nil), store[k]...)
	store[k][len(store[k])-1] ^= 1
	stream = build(t, func(w *Writer) { w.AddRef(KindExtentStream, childDigest, childSize, 0, 5) })
	if err := Reconstruct(context.Background(), bytes.NewReader(stream), store, io.Discard); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("a corrupted child gave %v", err)
	}
}

func TestNestingDepthIsBounded(t *testing.T) {
	store := memStore{}
	digest, size := store.add(build(t, func(w *Writer) { w.AddData([]byte("x")) }))
	for i := 0; i < maxDepth+2; i++ {
		d, s := digest, size
		digest, size = store.add(build(t, func(w *Writer) { w.AddRef(KindExtentStream, d, s, 0, 1) }))
	}
	stream := build(t, func(w *Writer) { w.AddRef(KindExtentStream, digest, size, 0, 1) })
	if err := Reconstruct(context.Background(), bytes.NewReader(stream), store, io.Discard); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Errorf("deep nesting gave %v", err)
	}
}

func TestDirStore(t *testing.T) {
	dir := t.TempDir()
	blob := []byte("from a directory")
	sum := sha256.Sum256(blob)
	path := casdirlayout.Path(filepath.Join(dir, "sha256"), hexString(sum[:]))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	stream := build(t, func(w *Writer) { w.AddRef(KindCAS, sum[:], int64(len(blob)), 5, 6) })
	if got := reconstruct(t, stream, compactstream.NewDirStore(dir)); string(got) != "a dire" {
		t.Errorf("got %q", got)
	}
}

func hexString(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}
