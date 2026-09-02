package compactstream

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"
)

// partsOf builds one part per string, each delivering exactly its bytes.
func partsOf(opened *[]int, chunks ...string) []BlobStreamPart {
	parts := make([]BlobStreamPart, len(chunks))
	for i, chunk := range chunks {
		parts[i] = BlobStreamPart{
			Size: int64(len(chunk)),
			Open: func() (io.ReadCloser, error) {
				if opened != nil {
					*opened = append(*opened, i)
				}
				return io.NopCloser(strings.NewReader(chunk)), nil
			},
		}
	}
	return parts
}

func TestBlobStreamConcatenatesParts(t *testing.T) {
	stream := NewBlobStream(partsOf(nil, "alpha", "", "beta", "gamma"))
	defer stream.Close()

	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if want := "alphabetagamma"; string(got) != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
}

func TestBlobStreamOpensPartsOnlyWhenReached(t *testing.T) {
	var opened []int
	stream := NewBlobStream(partsOf(&opened, "alpha", "beta", "gamma"))
	defer stream.Close()

	if len(opened) != 0 {
		t.Fatalf("parts %v were opened before the first read", opened)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "alpha" {
		t.Fatalf("read %q, want %q", buf, "alpha")
	}
	if len(opened) != 1 || opened[0] != 0 {
		t.Fatalf("opened %v after reading the first part, want [0]", opened)
	}
}

// A part must not be read past its declared size: the same reader may be
// serving whatever comes next.
func TestBlobStreamStopsAtThePartBoundary(t *testing.T) {
	shared := strings.NewReader("firstsecond")
	parts := []BlobStreamPart{
		{Size: 5, Open: func() (io.ReadCloser, error) { return io.NopCloser(shared), nil }},
		{Size: 6, Open: func() (io.ReadCloser, error) { return io.NopCloser(shared), nil }},
	}
	stream := NewBlobStream(parts)
	defer stream.Close()

	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if want := "firstsecond"; string(got) != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
}

func TestBlobStreamRejectsAShortPart(t *testing.T) {
	parts := []BlobStreamPart{
		{Size: 5, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("abc")), nil }},
	}
	stream := NewBlobStream(parts)
	defer stream.Close()

	_, err := io.ReadAll(stream)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want an unexpected-EOF error", err)
	}
}

func TestBlobStreamReportsAnOpenFailureOnce(t *testing.T) {
	want := errors.New("no such blob")
	parts := []BlobStreamPart{
		{Size: 3, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("abc")), nil }},
		{Size: 3, Open: func() (io.ReadCloser, error) { return nil, want }},
	}
	stream := NewBlobStream(parts)
	defer stream.Close()

	if _, err := io.ReadAll(stream); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	// The failure is sticky: a second read reports it again rather than moving on.
	if _, err := stream.Read(make([]byte, 1)); !errors.Is(err, want) {
		t.Fatalf("error after failure = %v, want %v", err, want)
	}
}

func TestBlobStreamClosesEachPart(t *testing.T) {
	var closed int
	part := func(content string) BlobStreamPart {
		return BlobStreamPart{
			Size: int64(len(content)),
			Open: func() (io.ReadCloser, error) {
				return &countingCloser{Reader: strings.NewReader(content), closed: &closed}, nil
			},
		}
	}
	stream := NewBlobStream([]BlobStreamPart{part("one"), part("two")})
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if closed != 2 {
		t.Fatalf("closed %d parts, want 2", closed)
	}
}

// Closing early must release the open part and not open any more.
func TestBlobStreamCloseStopsEarly(t *testing.T) {
	var opened []int
	var closed int
	parts := partsOf(&opened, "alpha", "beta")
	parts[0].Open = func() (io.ReadCloser, error) {
		opened = append(opened, 0)
		return &countingCloser{Reader: strings.NewReader("alpha"), closed: &closed}, nil
	}
	stream := NewBlobStream(parts)

	if _, err := io.ReadFull(stream, make([]byte, 2)); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if closed != 1 {
		t.Fatalf("closed %d parts, want 1", closed)
	}
	if _, err := io.ReadAll(stream); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 {
		t.Fatalf("opened %v, want only the first part", opened)
	}
}

// multiMapBlobStore serves blobs from memory and records the request lists it
// was handed, so a test can tell how reconstruction asked for them.
type multiMapBlobStore struct {
	blobs mapBlobStore

	multiCalls  [][]BlobRequest
	singleReads int
}

func (s *multiMapBlobStore) ReaderForBlob(ctx context.Context, digest []byte, size int64) (io.ReadCloser, error) {
	s.singleReads++
	return s.blobs.ReaderForBlob(ctx, digest, size)
}

func (s *multiMapBlobStore) ReaderForBlobs(ctx context.Context, requests []BlobRequest) (io.ReadCloser, error) {
	s.multiCalls = append(s.multiCalls, requests)
	parts := make([]BlobStreamPart, len(requests))
	for i, request := range requests {
		parts[i] = BlobStreamPart{
			Size: request.Size,
			Open: func() (io.ReadCloser, error) {
				return s.blobs.ReaderForBlob(ctx, request.Digest, request.Size)
			},
		}
	}
	return NewBlobStream(parts), nil
}

// buildBlobbyStream builds a compact stream of n small blobs separated by
// one-byte gaps, and returns the index together with the bytes it reconstructs
// to.
func buildBlobbyStream(store mapBlobStore, n int) (index, want []byte) {
	var refs []rawRef
	var stream []byte
	var offset uint64
	for i := range n {
		gap := []byte{byte(i)}
		blob := bytes.Repeat([]byte{byte(i), byte(i >> 8)}, 8)
		digest := store.put(blob)

		stream = append(stream, gap...)
		want = append(want, gap...)
		offset += uint64(len(gap))
		refs = append(refs, rawRef{offset: offset, digest: digest, size: uint64(len(blob))})
		want = append(want, blob...)
		offset += uint64(len(blob))
	}
	return buildRawCompactStream(refs, stream), want
}

// Reconstruction must hand a MultiBlobStore the whole reference table in one
// call: seeing every blob it will need is what lets the store batch them.
func TestReconstructAsksAMultiBlobStoreForTheWholeRefTable(t *testing.T) {
	store := &multiMapBlobStore{blobs: mapBlobStore{}}
	const refCount = 200
	index, want := buildBlobbyStream(store.blobs, refCount)

	var out bytes.Buffer
	if err := Reconstruct(context.Background(), bytes.NewReader(index), store, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("reconstruction through a MultiBlobStore changed the output")
	}
	if len(store.multiCalls) != 1 {
		t.Fatalf("ReaderForBlobs was called %d times, want 1", len(store.multiCalls))
	}
	if got := len(store.multiCalls[0]); got != refCount {
		t.Fatalf("ReaderForBlobs was asked for %d blobs, want %d", got, refCount)
	}
	if store.singleReads != 0 {
		t.Fatalf("%d blobs were read one at a time", store.singleReads)
	}
}

// The same stream must reconstruct identically whether the store can serve a
// list or only one blob at a time.
func TestReconstructMatchesBetweenSingleAndMultiBlobStores(t *testing.T) {
	blobs := mapBlobStore{}
	index, want := buildBlobbyStream(blobs, 64)

	var serial, multi bytes.Buffer
	if err := Reconstruct(context.Background(), bytes.NewReader(index), blobs, &serial); err != nil {
		t.Fatal(err)
	}
	if err := Reconstruct(context.Background(), bytes.NewReader(index), &multiMapBlobStore{blobs: blobs}, &multi); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(serial.Bytes(), want) {
		t.Fatal("serial reconstruction does not match the expected bytes")
	}
	if !bytes.Equal(multi.Bytes(), serial.Bytes()) {
		t.Fatal("multi-blob reconstruction differs from the serial one")
	}
}

// Whatever the layout of gaps and blobs, the two kinds of store must produce
// the same bytes: the only thing a MultiBlobStore changes is how the blobs are
// fetched.
func TestReconstructMatchesForRandomLayouts(t *testing.T) {
	for seed := range int64(50) {
		rng := rand.New(rand.NewSource(seed))
		blobs := mapBlobStore{}
		var refs []rawRef
		var stream, want []byte
		var offset uint64

		for range rng.Intn(60) {
			gap := make([]byte, rng.Intn(5))
			rng.Read(gap)
			stream = append(stream, gap...)
			want = append(want, gap...)
			offset += uint64(len(gap))

			blob := make([]byte, rng.Intn(3000))
			rng.Read(blob)
			digest := blobs.put(blob)
			refs = append(refs, rawRef{offset: offset, digest: digest, size: uint64(len(blob))})
			want = append(want, blob...)
			offset += uint64(len(blob))
		}
		tail := make([]byte, rng.Intn(16))
		rng.Read(tail)
		stream = append(stream, tail...)
		want = append(want, tail...)

		index := buildRawCompactStream(refs, stream)
		var serial, multi bytes.Buffer
		if err := Reconstruct(context.Background(), bytes.NewReader(index), blobs, &serial); err != nil {
			t.Fatalf("seed %d, serial: %v", seed, err)
		}
		if err := Reconstruct(context.Background(), bytes.NewReader(index), &multiMapBlobStore{blobs: blobs}, &multi); err != nil {
			t.Fatalf("seed %d, multi: %v", seed, err)
		}
		if !bytes.Equal(serial.Bytes(), want) {
			t.Fatalf("seed %d: serial reconstruction does not match the expected bytes", seed)
		}
		if !bytes.Equal(multi.Bytes(), serial.Bytes()) {
			t.Fatalf("seed %d: multi-blob reconstruction differs from the serial one", seed)
		}
	}
}

func TestReconstructReportsAMultiBlobStoreFailure(t *testing.T) {
	blobs := mapBlobStore{}
	index, _ := buildBlobbyStream(blobs, 4)
	// Drop one blob so the store cannot serve the whole list.
	for key := range blobs {
		delete(blobs, key)
		break
	}

	var out bytes.Buffer
	err := Reconstruct(context.Background(), bytes.NewReader(index), &multiMapBlobStore{blobs: blobs}, &out)
	if err == nil {
		t.Fatal("reconstruction succeeded with a blob missing from the store")
	}
	if !strings.Contains(err.Error(), "blob not found") {
		t.Fatalf("error = %v, want it to name the missing blob", err)
	}
}

type countingCloser struct {
	io.Reader
	closed *int
}

func (c *countingCloser) Close() error {
	*c.closed++
	return nil
}
