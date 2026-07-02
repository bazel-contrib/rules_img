package compactstream

import (
	"bytes"
	"context"
	"io"
	"testing"
)

// TestReconstructUncompressedInterleaves verifies the uncompressed reconstruction
// interleaves byte-stream gaps with real CAS blobs, just like Reconstruct but
// without any re-compression.
func TestReconstructUncompressedInterleaves(t *testing.T) {
	store := mapBlobStore{}
	blob := store.put([]byte("BLOBDATA")) // 8 bytes, fills [4,12)
	idx := buildRawCompactStream([]rawRef{{offset: 4, digest: blob, size: 8}}, []byte("HDR--END"))

	var out bytes.Buffer
	if err := ReconstructUncompressed(context.Background(), bytes.NewReader(idx), store, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "HDR-BLOBDATA-END"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestNullBlobStoreZeroFills verifies the fake-reader path: the omitted CAS blob
// ranges are replaced by exactly as many NUL bytes as the blob was long, while
// the byte-stream gaps (tar headers, inlined small files) are preserved. This is
// what lets archive/tar walk every header without access to the content store.
func TestNullBlobStoreZeroFills(t *testing.T) {
	// Two refs of sizes 8 and 3, with byte-stream gaps around them.
	digest := bytes.Repeat([]byte{0xab}, 32)
	idx := buildRawCompactStream([]rawRef{
		{offset: 4, digest: digest, size: 8},  // fills [4,12)
		{offset: 15, digest: digest, size: 3}, // fills [15,18)
	}, []byte("HDR-" /*[0,4)*/ +"GAP" /*[12,15)*/ +"-END" /*[18,22)*/))

	var out bytes.Buffer
	if err := ReconstructUncompressed(context.Background(), bytes.NewReader(idx), NullBlobStore{}, &out); err != nil {
		t.Fatal(err)
	}

	want := []byte("HDR-")
	want = append(want, make([]byte, 8)...)
	want = append(want, []byte("GAP")...)
	want = append(want, make([]byte, 3)...)
	want = append(want, []byte("-END")...)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("zero-fill mismatch:\ngot  %q\nwant %q", out.Bytes(), want)
	}
}

// TestReconstructingReaderDigests verifies the reader exposes the recorded CAS
// digest for a ref-backed content range and tracks the output offset, which is
// how the mtree builder attaches sha256digest to entries without the blobs.
func TestReconstructingReaderDigests(t *testing.T) {
	store := mapBlobStore{}
	blob := store.put([]byte("BLOBDATA")) // 8 bytes, fills [4,12)
	idx := buildRawCompactStream([]rawRef{{offset: 4, digest: blob, size: 8}}, []byte("HDR--END"))

	rr, err := NewReconstructingReader(context.Background(), bytes.NewReader(idx), NullBlobStore{})
	if err != nil {
		t.Fatal(err)
	}
	defer rr.Close()

	// The recorded digest is available for the exact content range only.
	if d, ok := rr.RefDigestAt(4, 8); !ok || !bytes.Equal(d, blob) {
		t.Fatalf("RefDigestAt(4, 8) = %x, %v; want %x, true", d, ok, blob)
	}
	if _, ok := rr.RefDigestAt(4, 7); ok {
		t.Fatal("RefDigestAt with wrong size must not match")
	}
	if _, ok := rr.RefDigestAt(5, 8); ok {
		t.Fatal("RefDigestAt with wrong offset must not match")
	}

	// Reading yields the byte stream with the ref range zero-filled, and Offset
	// advances to the full reconstructed length.
	out, err := io.ReadAll(rr)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte("HDR-"), make([]byte, 8)...), []byte("-END")...)
	if !bytes.Equal(out, want) {
		t.Fatalf("got %q, want %q", out, want)
	}
	if rr.Offset() != 16 {
		t.Fatalf("Offset() = %d, want 16", rr.Offset())
	}
}
