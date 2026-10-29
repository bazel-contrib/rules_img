package compactstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"
)

func TestWriteImage(t *testing.T) {
	image := []byte("HEAD-aaaa-MID-bbbbbbbb-small-TAIL")
	// [5,9) is below the threshold and stays inline, [14,22) is referenced.
	store := mapBlobStore{}
	small := sha256.Sum256(image[5:9])
	big := store.put(image[14:22])
	ranges := []Range{
		{Offset: 5, Size: 4, Digest: small[:]},
		{Offset: 14, Size: 8, Digest: big},
		{Offset: 22, Size: 0},
	}

	for _, incremental := range []bool{false, true} {
		var out bytes.Buffer
		w := NewWriter(&out, HashAlgoSHA256, 32, StreamCompressionZstd, OriginalCompressionInfo{}, 0)
		if incremental {
			if err := w.CompressIncrementally(); err != nil {
				t.Fatal(err)
			}
		}
		if err := WriteImage(w, bytes.NewReader(image), int64(len(image)), ranges, 8); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}

		info, err := Inspect(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if len(info.Refs) != 1 || info.Refs[0].Offset != 14 || info.Refs[0].Size != 8 {
			t.Errorf("incremental=%v: refs %+v", incremental, info.Refs)
		}
		if int(info.StreamUncompressedSize) != len(image)-8 {
			t.Errorf("incremental=%v: inline bytes %d", incremental, info.StreamUncompressedSize)
		}
		var got bytes.Buffer
		if err := Reconstruct(context.Background(), bytes.NewReader(out.Bytes()), store, &got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), image) {
			t.Errorf("incremental=%v: reconstructed %q", incremental, got.Bytes())
		}
	}
}

func TestWriteImageErrors(t *testing.T) {
	image := bytes.Repeat([]byte{1}, 100)
	digest := bytes.Repeat([]byte{2}, 32)
	for name, ranges := range map[string][]Range{
		"unsorted":     {{Offset: 50, Size: 10, Digest: digest}, {Offset: 10, Size: 10, Digest: digest}},
		"overlapping":  {{Offset: 10, Size: 20, Digest: digest}, {Offset: 20, Size: 10, Digest: digest}},
		"out of range": {{Offset: 95, Size: 10, Digest: digest}},
	} {
		w := NewWriter(&bytes.Buffer{}, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)
		if err := WriteImage(w, bytes.NewReader(image), int64(len(image)), ranges, 1); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	w := NewWriter(&bytes.Buffer{}, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)
	if err := WriteImage(w, bytes.NewReader(image[:50]), 100, nil, 1); err == nil {
		t.Error("short image accepted")
	}
	w = NewWriter(&bytes.Buffer{}, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{Compression: OriginalCompressionGzip}, 0)
	if err := WriteImage(w, bytes.NewReader(image), 100, nil, 1); err == nil {
		t.Error("compressed original accepted")
	}
}
