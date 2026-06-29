package compactstream

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestWriterEmptyStream(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	if len(data) != headerSize {
		t.Fatalf("expected %d bytes (header only), got %d", headerSize, len(data))
	}

	if string(data[0:6]) != magic {
		t.Fatalf("bad magic: %q", data[0:6])
	}
	if data[6] != 0x00 || data[7] != FormatVersion {
		t.Fatalf("bad version bytes: %x %x", data[6], data[7])
	}
	if binary.BigEndian.Uint16(data[8:10]) != HashAlgoSHA256 {
		t.Fatalf("bad hash algo")
	}
	if binary.BigEndian.Uint16(data[10:12]) != 32 {
		t.Fatalf("bad hash size")
	}
	if data[12] != StreamCompressionNone {
		t.Fatalf("bad stream compression: %d", data[12])
	}

	refTableOffset := binary.BigEndian.Uint64(data[24:32])
	refTableSize := binary.BigEndian.Uint64(data[32:40])
	streamOffset := binary.BigEndian.Uint64(data[40:48])
	streamSize := binary.BigEndian.Uint64(data[48:56])

	if refTableOffset != headerSize {
		t.Fatalf("expected ref table offset %d, got %d", headerSize, refTableOffset)
	}
	if refTableSize != 0 {
		t.Fatalf("expected ref table size 0, got %d", refTableSize)
	}
	if streamOffset != headerSize {
		t.Fatalf("expected stream offset %d, got %d", headerSize, streamOffset)
	}
	if streamSize != 0 {
		t.Fatalf("expected stream size 0, got %d", streamSize)
	}
}

func TestWriterOriginalCompressionMetadata(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{
		Compression:      OriginalCompressionGzip,
		Seekable:         true,
		CompressionLevel: 6,
		CompressorJobs:   4,
	}, 0)
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	if data[13] != OriginalCompressionGzip {
		t.Fatalf("expected original compression gzip (%d), got %d", OriginalCompressionGzip, data[13])
	}
	if data[14] != 1 {
		t.Fatalf("expected seekable=1, got %d", data[14])
	}
	if int8(data[15]) != 6 {
		t.Fatalf("expected compression level 6, got %d", int8(data[15]))
	}
	if data[16] != 4 {
		t.Fatalf("expected compressor jobs 4, got %d", data[16])
	}
}

func TestWriterEndPadding(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{
		EndPadding: 1024,
	}, 0)
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	endPadding := binary.BigEndian.Uint32(data[20:24])
	if endPadding != 1024 {
		t.Fatalf("expected end padding 1024, got %d", endPadding)
	}
}

func TestWriterStreamBytesOnly(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)

	streamData := []byte("hello world stream data")
	if err := iw.WriteStreamBytes(streamData); err != nil {
		t.Fatal(err)
	}
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	refTableSize := binary.BigEndian.Uint64(data[32:40])
	streamOffset := binary.BigEndian.Uint64(data[40:48])
	streamSize := binary.BigEndian.Uint64(data[48:56])

	if refTableSize != 0 {
		t.Fatalf("expected ref table size 0, got %d", refTableSize)
	}
	if streamSize != uint64(len(streamData)) {
		t.Fatalf("expected stream size %d, got %d", len(streamData), streamSize)
	}

	storedStream := data[streamOffset : streamOffset+streamSize]
	if !bytes.Equal(storedStream, streamData) {
		t.Fatalf("stream data mismatch")
	}
}

func TestWriterSingleCASRef(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)

	prefix := []byte("prefix-bytes-")
	if err := iw.WriteStreamBytes(prefix); err != nil {
		t.Fatal(err)
	}

	content := []byte("cas content here")
	digest := sha256.Sum256(content)
	if err := iw.WriteCASRef(digest[:], uint64(len(content))); err != nil {
		t.Fatal(err)
	}

	suffix := []byte("-suffix-bytes")
	if err := iw.WriteStreamBytes(suffix); err != nil {
		t.Fatal(err)
	}

	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	refTableOffset := binary.BigEndian.Uint64(data[24:32])
	refTableSize := binary.BigEndian.Uint64(data[32:40])
	streamOffset := binary.BigEndian.Uint64(data[40:48])
	streamSize := binary.BigEndian.Uint64(data[48:56])

	// Ref table should have exactly one entry: 8 + 32 + 8 = 48 bytes
	if refTableSize != 48 {
		t.Fatalf("expected ref table size 48, got %d", refTableSize)
	}

	// Parse the ref entry
	refData := data[refTableOffset : refTableOffset+refTableSize]
	refOffset := binary.BigEndian.Uint64(refData[0:8])
	refDigest := refData[8:40]
	refSize := binary.BigEndian.Uint64(refData[40:48])

	if refOffset != uint64(len(prefix)) {
		t.Fatalf("expected ref offset %d, got %d", len(prefix), refOffset)
	}
	if !bytes.Equal(refDigest, digest[:]) {
		t.Fatalf("ref digest mismatch")
	}
	if refSize != uint64(len(content)) {
		t.Fatalf("expected ref size %d, got %d", len(content), refSize)
	}

	// Stream should contain prefix + suffix (no CAS content)
	expectedStream := append(prefix, suffix...)
	storedStream := data[streamOffset : streamOffset+streamSize]
	if !bytes.Equal(storedStream, expectedStream) {
		t.Fatalf("stream data mismatch: got %q, want %q", storedStream, expectedStream)
	}
}

func TestWriterMultipleCASRefs(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)

	// Stream: [header1][CAS1][padding1][header2][CAS2][padding2]
	header1 := bytes.Repeat([]byte("H"), 512)
	if err := iw.WriteStreamBytes(header1); err != nil {
		t.Fatal(err)
	}

	content1 := []byte("first file content")
	digest1 := sha256.Sum256(content1)
	if err := iw.WriteCASRef(digest1[:], uint64(len(content1))); err != nil {
		t.Fatal(err)
	}

	padding1 := make([]byte, 512-len(content1)%512)
	if err := iw.WriteStreamBytes(padding1); err != nil {
		t.Fatal(err)
	}

	header2 := bytes.Repeat([]byte("I"), 512)
	if err := iw.WriteStreamBytes(header2); err != nil {
		t.Fatal(err)
	}

	content2 := []byte("second file content that is different")
	digest2 := sha256.Sum256(content2)
	if err := iw.WriteCASRef(digest2[:], uint64(len(content2))); err != nil {
		t.Fatal(err)
	}

	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	refTableSize := binary.BigEndian.Uint64(data[32:40])

	// Two entries: 2 * 48 = 96 bytes
	if refTableSize != 96 {
		t.Fatalf("expected ref table size 96, got %d", refTableSize)
	}
}

func TestWriterDigestLengthValidation(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)

	badDigest := make([]byte, 16)
	err := iw.WriteCASRef(badDigest, 100)
	if err == nil {
		t.Fatal("expected error for wrong digest length")
	}
}

func TestWriterZstdStreamCompression(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionZstd, OriginalCompressionInfo{}, 0)

	streamData := bytes.Repeat([]byte("compressible data "), 100)
	if err := iw.WriteStreamBytes(streamData); err != nil {
		t.Fatal(err)
	}

	content := []byte("cas blob")
	digest := sha256.Sum256(content)
	if err := iw.WriteCASRef(digest[:], uint64(len(content))); err != nil {
		t.Fatal(err)
	}

	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	if data[12] != StreamCompressionZstd {
		t.Fatalf("expected stream compression zstd")
	}

	streamOffset := binary.BigEndian.Uint64(data[40:48])
	streamSize := binary.BigEndian.Uint64(data[48:56])

	// Decompress and verify
	dec, err := zstd.NewReader(bytes.NewReader(data[streamOffset : streamOffset+streamSize]))
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()

	decompressed, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(decompressed, streamData) {
		t.Fatalf("decompressed stream mismatch")
	}

	// Verify compression actually reduced size
	if streamSize >= uint64(len(streamData)) {
		t.Fatalf("expected compressed size < uncompressed size, got %d >= %d", streamSize, len(streamData))
	}
}

func TestWriterDefaultCompressionLevel(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{
		Compression:      OriginalCompressionGzip,
		CompressionLevel: -1,
	}, 0)
	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	if int8(data[15]) != -1 {
		t.Fatalf("expected compression level -1, got %d", int8(data[15]))
	}
}

func TestWriterInlineThreshold(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 4096)

	if iw.InlineThreshold() != 4096 {
		t.Fatalf("expected inline threshold 4096, got %d", iw.InlineThreshold())
	}
}

func TestHeaderBytesCanBeParsed(t *testing.T) {
	headers := []*tar.Header{
		{Typeflag: tar.TypeDir, Name: "dir/", Mode: 0o755},
		{Typeflag: tar.TypeReg, Name: "file.txt", Size: 100, Mode: 0o644},
		{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "file.txt"},
		{Typeflag: tar.TypeLink, Name: "hardlink", Linkname: "file.txt", Size: 0},
		{Typeflag: tar.TypeReg, Name: "empty", Size: 0, Mode: 0o755},
	}

	for _, hdr := range headers {
		headerBytes, err := CaptureTarHeaderBytes(hdr)
		if err != nil {
			t.Fatalf("CaptureTarHeaderBytes(%s): %v", hdr.Name, err)
		}
		if len(headerBytes) == 0 {
			t.Fatalf("empty header bytes for %s", hdr.Name)
		}
		if len(headerBytes)%512 != 0 {
			t.Fatalf("header bytes for %s not 512-aligned: %d", hdr.Name, len(headerBytes))
		}

		tr := tar.NewReader(bytes.NewReader(headerBytes))
		parsed, err := tr.Next()
		if err != nil {
			t.Fatalf("parsing header bytes for %s: %v", hdr.Name, err)
		}
		if parsed.Typeflag != hdr.Typeflag {
			t.Fatalf("typeflag mismatch for %s: got %d, want %d", hdr.Name, parsed.Typeflag, hdr.Typeflag)
		}
		if parsed.Name != hdr.Name {
			t.Fatalf("name mismatch: got %q, want %q", parsed.Name, hdr.Name)
		}
		if parsed.Typeflag == tar.TypeReg {
			if parsed.Size != hdr.Size {
				t.Fatalf("size mismatch for %s: got %d, want %d", hdr.Name, parsed.Size, hdr.Size)
			}
		}
	}
}

func TestWriterCASRefsSortedByOffset(t *testing.T) {
	var buf bytes.Buffer
	iw := NewWriter(&buf, HashAlgoSHA256, 32, StreamCompressionNone, OriginalCompressionInfo{}, 0)

	// Write stream+CAS pattern: [stream][cas][stream][cas][stream]
	for i := 0; i < 3; i++ {
		streamChunk := bytes.Repeat([]byte{byte('A' + i)}, 100)
		if err := iw.WriteStreamBytes(streamChunk); err != nil {
			t.Fatal(err)
		}
		if i < 2 {
			content := bytes.Repeat([]byte{byte('a' + i)}, 50)
			digest := sha256.Sum256(content)
			if err := iw.WriteCASRef(digest[:], uint64(len(content))); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := iw.Close(); err != nil {
		t.Fatal(err)
	}

	data := buf.Bytes()
	refTableOffset := binary.BigEndian.Uint64(data[24:32])
	refTableSize := binary.BigEndian.Uint64(data[32:40])

	entrySize := 48 // 8 + 32 + 8
	refCount := int(refTableSize) / entrySize
	if refCount != 2 {
		t.Fatalf("expected 2 refs, got %d", refCount)
	}

	var prevOffset uint64
	for i := 0; i < refCount; i++ {
		base := refTableOffset + uint64(i*entrySize)
		offset := binary.BigEndian.Uint64(data[base : base+8])
		if i > 0 && offset <= prevOffset {
			t.Fatalf("refs not sorted: offset[%d]=%d <= offset[%d]=%d", i, offset, i-1, prevOffset)
		}
		prevOffset = offset
	}
}
