package compactstream

import (
	"bytes"
	"context"
	"math/rand"
	"strings"
	"testing"
)

type streamSegment struct {
	inline []byte // inline bytes, or
	blob   []byte // a reference to this blob
}

// buildStream writes a compact stream of the segments, and returns it with what
// it reconstructs to and the inline intervals of that content.
func buildStream(t *testing.T, store mapBlobStore, segs []streamSegment, mutate func(*Writer)) (stream, content []byte, inline [][2]int64) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionZstd, OriginalCompressionInfo{}, 0)
	if err := w.CompressIncrementally(); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(w)
	}
	for _, s := range segs {
		if s.blob != nil {
			if err := w.WriteCASRef(store.put(s.blob), uint64(len(s.blob))); err != nil {
				t.Fatal(err)
			}
			content = append(content, s.blob...)
			continue
		}
		if err := w.WriteStreamBytes(s.inline); err != nil {
			t.Fatal(err)
		}
		if len(s.inline) > 0 {
			inline = append(inline, [2]int64{int64(len(content)), int64(len(content) + len(s.inline))})
		}
		content = append(content, s.inline...)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), content, inline
}

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	r.Read(b)
	return b
}

func reconstruct(t *testing.T, stream []byte, store BlobStore) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := Reconstruct(context.Background(), bytes.NewReader(stream), store, &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestAppendStreamProperty(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for iter := 0; iter < 40; iter++ {
		store := mapBlobStore{}
		var disk bytes.Buffer
		w := NewWriter(&disk, HashAlgoSHA256, 32, StreamCompressionZstd, OriginalCompressionInfo{}, 0)
		if err := w.CompressIncrementally(); err != nil {
			t.Fatal(err)
		}
		var want []byte
		for part := 0; part < 1+r.Intn(5); part++ {
			switch r.Intn(5) {
			case 0: // zeros
				n := int64(r.Intn(3 << 20))
				if err := w.WriteZeroRun(n); err != nil {
					t.Fatal(err)
				}
				want = append(want, make([]byte, n)...)
			case 1: // an inline image with a patch
				img := randBytes(r, 1+r.Intn(5000))
				off := int64(r.Intn(len(img)))
				patch := Patch{Offset: off, Data: randBytes(r, 1+r.Intn(len(img)-int(off)))}
				if err := AppendImage(w, bytes.NewReader(img), int64(len(img)), []Patch{patch}); err != nil {
					t.Fatal(err)
				}
				copy(img[off:], patch.Data)
				want = append(want, img...)
			case 2: // an image as one reference
				img := randBytes(r, 1+r.Intn(70000))
				digest, err := AppendImageRef(w, bytes.NewReader(img), int64(len(img)))
				if err != nil {
					t.Fatal(err)
				}
				store.put(img)
				if string(digest) != string(store.put(img)) {
					t.Fatal("wrong digest")
				}
				want = append(want, img...)
			default: // another compact stream, with patches in its inline bytes
				var segs []streamSegment
				for i := 0; i < r.Intn(6); i++ {
					if r.Intn(2) == 0 {
						segs = append(segs, streamSegment{inline: randBytes(r, r.Intn(3000))})
					} else {
						segs = append(segs, streamSegment{blob: randBytes(r, 1+r.Intn(40000))})
					}
				}
				stream, content, inline := buildStream(t, store, segs, func(w *Writer) {
					if r.Intn(2) == 0 {
						w.SetContentFormat(ContentFormatErofs)
					}
				})
				var patches []Patch
				for _, iv := range inline {
					if r.Intn(2) == 0 && iv[1]-iv[0] >= 2 {
						off := iv[0] + int64(r.Intn(int(iv[1]-iv[0]-1)))
						n := 1 + r.Intn(int(iv[1]-off))
						patches = append(patches, Patch{Offset: off, Data: randBytes(r, n)})
						copy(content[off:], patches[len(patches)-1].Data)
					}
				}
				n, err := AppendStream(w, bytes.NewReader(stream), patches)
				if err != nil {
					t.Fatalf("iteration %d: %v", iter, err)
				}
				if n != int64(len(content)) {
					t.Fatalf("AppendStream returned %d, want %d", n, len(content))
				}
				want = append(want, content...)
			}
			if w.Offset() != int64(len(want)) {
				t.Fatalf("iteration %d: writer at %d, content has %d bytes", iter, w.Offset(), len(want))
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if got := reconstruct(t, disk.Bytes(), store); !bytes.Equal(got, want) {
			t.Fatalf("iteration %d: the reconstructed stream differs (%d bytes, want %d)", iter, len(got), len(want))
		}
		if size, err := ReconstructedSize(bytes.NewReader(disk.Bytes())); err != nil || size != int64(len(want)) {
			t.Fatalf("ReconstructedSize = %d, %v; want %d", size, err, len(want))
		}
	}
}

func TestAppendStreamKeepsReferences(t *testing.T) {
	store := mapBlobStore{}
	blob := bytes.Repeat([]byte("file content "), 5000)
	stream, content, _ := buildStream(t, store, []streamSegment{{inline: []byte("header")}, {blob: blob}, {inline: []byte("trailer")}}, nil)

	var disk bytes.Buffer
	w := NewWriter(&disk, HashAlgoSHA256, 32, StreamCompressionZstd, OriginalCompressionInfo{}, 0)
	w.WriteStreamBytes(make([]byte, 1000))
	if _, err := AppendStream(w, bytes.NewReader(stream), nil); err != nil {
		t.Fatal(err)
	}
	w.WriteZeroRun(500)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// The blob is a reference of the new stream, at its new offset, and is not
	// part of the inline bytes.
	info, err := Inspect(bytes.NewReader(disk.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Refs) != 1 || info.Refs[0].Offset != 1000+6 || info.Refs[0].Size != uint64(len(blob)) {
		t.Fatalf("references: %+v", info.Refs)
	}
	if info.StreamUncompressedSize != 1000+6+7+500 {
		t.Errorf("%d inline bytes", info.StreamUncompressedSize)
	}
	want := append(append(make([]byte, 1000), content...), make([]byte, 500)...)
	if got := reconstruct(t, disk.Bytes(), store); !bytes.Equal(got, want) {
		t.Error("wrong content")
	}
}

func TestAppendStreamErrors(t *testing.T) {
	store := mapBlobStore{}
	blob := bytes.Repeat([]byte{7}, 10000)
	stream, _, _ := buildStream(t, store, []streamSegment{{inline: []byte("0123456789")}, {blob: blob}, {inline: []byte("abcdefghij")}}, nil)
	newWriter := func() *Writer {
		return NewWriter(&bytes.Buffer{}, HashAlgoSHA256, 32, StreamCompressionZstd, OriginalCompressionInfo{}, 0)
	}

	for name, patches := range map[string][]Patch{
		"on the reference":      {{Offset: 100, Data: []byte("x")}},
		"across the reference":  {{Offset: 8, Data: []byte("xxxx")}},
		"beyond the content":    {{Offset: 10 + 10000 + 8, Data: []byte("xxxx")}},
		"overlapping":           {{Offset: 1, Data: []byte("xxx")}, {Offset: 2, Data: []byte("x")}},
		"negative":              {{Offset: -1, Data: []byte("x")}},
		"after the end":         {{Offset: 1 << 30, Data: []byte("x")}},
		"across the end of all": {{Offset: 10 + 10000 + 9, Data: []byte("xx")}},
	} {
		if _, err := AppendStream(newWriter(), bytes.NewReader(stream), patches); err == nil {
			t.Errorf("patch %s accepted", name)
		}
	}
	// Patches in the inline bytes are fine, up to the last byte.
	if _, err := AppendStream(newWriter(), bytes.NewReader(stream), []Patch{{Offset: 0, Data: []byte("x")}, {Offset: 10 + 10000 + 9, Data: []byte("y")}}); err != nil {
		t.Errorf("valid patches: %v", err)
	}

	var gz bytes.Buffer
	gw := NewWriter(&gz, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{Compression: OriginalCompressionGzip}, 0)
	gw.WriteStreamBytes([]byte("data"))
	gw.Close()
	if _, err := AppendStream(newWriter(), bytes.NewReader(gz.Bytes()), nil); err == nil || !strings.Contains(err.Error(), "compressed") {
		t.Errorf("a stream of a compressed file: %v", err)
	}

	var tarStream bytes.Buffer
	tw := NewWriter(&tarStream, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)
	tw.SetContentFormat(ContentFormatTar)
	tw.Close()
	if _, err := AppendStream(newWriter(), bytes.NewReader(tarStream.Bytes()), nil); err == nil || !strings.Contains(err.Error(), "tar") {
		t.Errorf("a tar stream: %v", err)
	}

	var small bytes.Buffer
	sw := NewWriter(&small, HashAlgoSHA256, 16, StreamCompressionNone, OriginalCompressionInfo{}, 0)
	sw.Close()
	if _, err := AppendStream(newWriter(), bytes.NewReader(small.Bytes()), nil); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Errorf("a stream with another hash size: %v", err)
	}

	if _, err := AppendStream(newWriter(), strings.NewReader("not a compact stream"), nil); err == nil {
		t.Error("garbage accepted")
	}
	// A truncated stream is an error, not a short result.
	if _, err := AppendStream(newWriter(), bytes.NewReader(stream[:len(stream)-3]), nil); err == nil {
		t.Error("truncated stream accepted")
	}

	// End padding is appended as zeros.
	var padded bytes.Buffer
	pw := NewWriter(&padded, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{EndPadding: 5}, 0)
	pw.WriteStreamBytes([]byte("ab"))
	pw.Close()
	var out bytes.Buffer
	w := NewWriter(&out, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)
	if n, err := AppendStream(w, bytes.NewReader(padded.Bytes()), nil); err != nil || n != 7 {
		t.Fatalf("padded stream: %d, %v", n, err)
	}
	w.Close()
	if got := reconstruct(t, out.Bytes(), mapBlobStore{}); string(got) != "ab\x00\x00\x00\x00\x00" {
		t.Errorf("padded content %q", got)
	}
}

func TestWriteZeroRunAndImages(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionZstd, OriginalCompressionInfo{}, 0)
	if err := w.CompressIncrementally(); err != nil {
		t.Fatal(err)
	}
	// A 300 MiB run of zeros costs next to nothing on disk.
	if err := w.WriteZeroRun(300 << 20); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteZeroRun(-1); err == nil {
		t.Error("negative run accepted")
	}
	w2 := NewWriter(&bytes.Buffer{}, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)
	if err := AppendImage(w2, strings.NewReader("short"), 10, nil); err == nil {
		t.Error("short image accepted")
	}
	if _, err := AppendImageRef(w2, strings.NewReader("short"), 10); err == nil {
		t.Error("short image accepted by AppendImageRef")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if buf.Len() > 64<<10 {
		t.Errorf("the stream is %d bytes", buf.Len())
	}
	info, err := Inspect(bytes.NewReader(buf.Bytes()))
	if err != nil || info.StreamUncompressedSize != 300<<20 {
		t.Errorf("%+v, %v", info, err)
	}
}
