package compactstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
)

// transformedFixture builds the content "head" + encode(file) + "tail" through
// the plan splitter and returns the compact stream, the reconstructed content and
// the store holding the source file.
func transformedFixture(t *testing.T, kind uint8, clusterBlocks int) (stream, content []byte, store mapBlobStore, file []byte) {
	t.Helper()
	file = bytes.Repeat([]byte("compressible text for the transform test. "), 4000)
	store = mapBlobStore{}
	digest := store.put(file)

	var encoded bytes.Buffer
	if _, err := erofs.EncodeFileClusters(&encoded, bytes.NewReader(file), int64(len(file)), 4096, clusterBlocks, algorithmOf(kind)); err != nil {
		t.Fatal(err)
	}
	content = append(append([]byte("head"), encoded.Bytes()...), "tail"...)

	var out bytes.Buffer
	w := NewWriter(&out, HashAlgoSHA256, sha256.Size, StreamCompressionNone, OriginalCompressionInfo{}, 0)
	var sink bytes.Buffer
	regions := []Region{{
		Offset: 4,
		Size:   int64(encoded.Len()),
		Digest: digest,
		Transform: &Transform{
			Kind:          kind,
			BlockBits:     12,
			ClusterBlocks: uint16(clusterBlocks),
			SourceSize:    uint64(len(file)),
		},
	}}
	split, err := NewPlanSplitter(w, &sink, regions, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := split.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := split.Finish(int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), content, store, file
}

func algorithmOf(kind uint8) erofs.CompressionAlgorithm {
	switch kind {
	case TransformErofsZstd:
		return erofs.CompressionZstd
	case TransformErofsDeflate:
		return erofs.CompressionDeflate
	}
	return erofs.CompressionLZ4
}

func TestTransformedRefRoundTrip(t *testing.T) {
	for _, kind := range []uint8{TransformErofsLZ4, TransformErofsDeflate, TransformErofsZstd} {
		for _, cb := range []int{1, 8} {
			t.Run(fmt.Sprintf("%d-%d", kind, cb), func(t *testing.T) { testTransformedRefRoundTrip(t, kind, cb) })
		}
	}
}

func testTransformedRefRoundTrip(t *testing.T, kind uint8, clusterBlocks int) {
	stream, content, store, file := transformedFixture(t, kind, clusterBlocks)

	info, err := Inspect(bytes.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if info.Header.Version != FormatVersionTransform || !info.Header.TransformedRefs {
		t.Fatalf("header = %+v, want a version-2 stream with transformed refs", info.Header)
	}
	if len(info.Refs) != 1 || info.Refs[0].Transform == nil {
		t.Fatalf("refs = %+v", info.Refs)
	}
	ref := info.Refs[0]
	if ref.BlobSize() != uint64(len(file)) || ref.Size >= ref.BlobSize() {
		t.Fatalf("ref size %d, blob size %d", ref.Size, ref.BlobSize())
	}
	if got, want := info.ReconstructedSize(), uint64(len(content)); got != want {
		t.Fatalf("ReconstructedSize = %d, want %d", got, want)
	}

	var out bytes.Buffer
	if err := Reconstruct(context.Background(), bytes.NewReader(stream), store, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), content) {
		t.Fatal("Reconstruct did not reproduce the content")
	}

	var viaReader bytes.Buffer
	if err := ReconstructUncompressed(context.Background(), bytes.NewReader(stream), store, &viaReader); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(viaReader.Bytes(), content) {
		t.Fatal("ReconstructUncompressed did not reproduce the content")
	}

	var zeroed bytes.Buffer
	if err := ReconstructUncompressed(context.Background(), bytes.NewReader(stream), NullBlobStore{}, &zeroed); err != nil {
		t.Fatal(err)
	}
	if zeroed.Len() != len(content) || !bytes.Equal(zeroed.Bytes()[:4], []byte("head")) || !bytes.Equal(zeroed.Bytes()[zeroed.Len()-4:], []byte("tail")) {
		t.Fatalf("zero-filled reconstruction has the wrong shape (%d bytes)", zeroed.Len())
	}
}

func TestTransformedRefDetectsDifferentOutput(t *testing.T) {
	stream, _, store, file := transformedFixture(t, TransformErofsLZ4, 1)
	// A different source file with the same digest key: the encoder output
	// differs from the recorded digest.
	for k := range store {
		store[k] = bytes.Repeat([]byte("x"), len(file))
	}
	var out bytes.Buffer
	err := Reconstruct(context.Background(), bytes.NewReader(stream), store, &out)
	if err == nil || !strings.Contains(err.Error(), "transform") {
		t.Fatalf("Reconstruct error = %v, want a transform mismatch", err)
	}
}

func TestPlainStreamsKeepVersionOne(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, HashAlgoSHA256, sha256.Size, StreamCompressionNone, OriginalCompressionInfo{}, 0)
	if err := w.WriteStreamBytes([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := out.Bytes()[7]; got != FormatVersion {
		t.Fatalf("version = %d, want %d", got, FormatVersion)
	}
}

func TestTransformedRefNeedsOutputDigest(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out, HashAlgoSHA256, sha256.Size, StreamCompressionNone, OriginalCompressionInfo{}, 0)
	err := w.WriteTransformedCASRef(make([]byte, sha256.Size), 4096, Transform{Kind: TransformErofsLZ4, BlockBits: 12, SourceSize: 10000})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded with a transformed reference that has no output digest")
	}
}

func TestVersionOneRejectsTransformFlag(t *testing.T) {
	var hdr [headerSize]byte
	copy(hdr[0:6], magic)
	hdr[7] = FormatVersion
	hdr[56] = flagTransformedRefs
	if _, err := parseHeader(hdr); err == nil {
		t.Fatal("a version-1 header with the transform flag was accepted")
	}
}

// TestReconstructingReaderDetectsDriftedEncoder checks the streaming reader
// notices output that differs from what the stream recorded even though the
// consumer stops after the expected number of bytes.
func TestReconstructingReaderDetectsDriftedEncoder(t *testing.T) {
	stream, _, store, file := transformedFixture(t, TransformErofsLZ4, 1)
	// Same length, different content: the encoder output differs but the
	// recorded size still matches.
	for k := range store {
		other := append([]byte(nil), file...)
		other[len(other)/2] ^= 0xff
		store[k] = other
	}
	var out bytes.Buffer
	err := ReconstructUncompressed(context.Background(), bytes.NewReader(stream), store, &out)
	if err == nil || !strings.Contains(err.Error(), "transform") {
		t.Fatalf("ReconstructUncompressed error = %v, want a transform mismatch", err)
	}
}
