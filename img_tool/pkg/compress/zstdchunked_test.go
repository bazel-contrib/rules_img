package compress

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

const (
	zstdChunkedManifestChecksumAnnotation = api.ZstdChunkedAnnotationPrefix + "manifest-checksum"
	zstdChunkedManifestPositionAnnotation = api.ZstdChunkedAnnotationPrefix + "manifest-position"
	zstdChunkedTarSplitPositionAnnotation = api.ZstdChunkedAnnotationPrefix + "tarsplit-position"
)

func TestResolveTarFormat(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		estargz     bool
		zstdChunked bool
		compression string
		want        TarFormat
		wantErr     bool
	}{
		{name: "plain gzip", compression: "gzip", want: TarFormatPlain},
		{name: "plain zstd", compression: "zstd", want: TarFormatPlain},
		{name: "estargz gzip", estargz: true, compression: "gzip", want: TarFormatEstargz},
		{name: "estargz zstd", estargz: true, compression: "zstd", want: TarFormatEstargz},
		{name: "zstd chunked", zstdChunked: true, compression: "zstd", want: TarFormatZstdChunked},
		{name: "zstd chunked with gzip", zstdChunked: true, compression: "gzip", wantErr: true},
		{name: "zstd chunked uncompressed", zstdChunked: true, compression: "uncompressed", wantErr: true},
		{name: "both formats", estargz: true, zstdChunked: true, compression: "zstd", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := ResolveTarFormat(tc.estargz, tc.zstdChunked, tc.compression)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got format %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got format %d, want %d", got, tc.want)
			}
		})
	}
}

func TestZstdChunkedBlob(t *testing.T) {
	t.Parallel()

	input, content := chunkedTestTar(t)

	var blob bytes.Buffer
	appender, err := NewSHA256ZstdChunkedTarAppender(&blob, ContentType("tar"))
	if err != nil {
		t.Fatalf("creating appender: %v", err)
	}
	if err := appender.AppendTar(bytes.NewReader(input)); err != nil {
		t.Fatalf("appending tar: %v", err)
	}
	state, err := appender.Finalize()
	if err != nil {
		t.Fatalf("finalizing: %v", err)
	}

	uncompressed := unzstd(t, blob.Bytes())
	if !bytes.Equal(uncompressed, input) {
		t.Errorf("decompressed blob differs from the appended tar: %d vs %d bytes", len(uncompressed), len(input))
	}
	if want := sha256.Sum256(input); !bytes.Equal(state.ContentHash, want[:]) {
		t.Errorf("content hash is %x, but the input tar hashes to %x", state.ContentHash, want)
	}
	if got, want := state.UncompressedSize, int64(len(input)); got != want {
		t.Errorf("uncompressed size is %d, want %d", got, want)
	}
	if want := sha256.Sum256(blob.Bytes()); !bytes.Equal(state.OuterHash, want[:]) {
		t.Errorf("outer hash is %x, but the blob hashes to %x", state.OuterHash, want)
	}
	if got, want := state.CompressedSize, int64(blob.Len()); got != want {
		t.Errorf("compressed size is %d, want %d", got, want)
	}

	for _, key := range []string{
		zstdChunkedManifestChecksumAnnotation,
		zstdChunkedManifestPositionAnnotation,
		zstdChunkedTarSplitPositionAnnotation,
		api.UncompressedSizeAnnotation,
	} {
		if _, ok := state.LayerAnnotations[key]; !ok {
			t.Errorf("missing layer annotation %s", key)
		}
	}
	if _, ok := state.LayerAnnotations[api.TocDigestAnnotation]; ok {
		t.Errorf("zstd:chunked layer must not carry the estargz annotation %s", api.TocDigestAnnotation)
	}
	if got, want := state.LayerAnnotations[api.UncompressedSizeAnnotation], fmt.Sprint(len(input)); got != want {
		t.Errorf("uncompressed size annotation is %s, want %s", got, want)
	}

	toc := readZstdChunkedTOC(t, blob.Bytes(), state.LayerAnnotations[zstdChunkedManifestPositionAnnotation])

	var chunks []tocEntry
	for _, e := range toc.Entries {
		if e.Name == "data.bin" && (e.Type == "reg" || e.Type == "chunk") {
			chunks = append(chunks, e)
		}
	}
	if len(chunks) < 2 {
		t.Fatalf("expected data.bin to be split into several chunks, got %d entries", len(chunks))
	}
	var covered int64
	sizes := map[int64]bool{}
	for i, e := range chunks {
		if e.ChunkOffset != covered {
			t.Errorf("chunk %d starts at %d, want %d", i, e.ChunkOffset, covered)
		}
		if e.ChunkSize <= 0 {
			t.Errorf("chunk %d has size %d", i, e.ChunkSize)
		}
		if got, want := e.ChunkDigest, fmt.Sprintf("sha256:%x", sha256.Sum256(content[e.ChunkOffset:e.ChunkOffset+e.ChunkSize])); got != want {
			t.Errorf("chunk %d digest is %s, want %s", i, got, want)
		}
		sizes[e.ChunkSize] = true
		covered += e.ChunkSize
	}
	if covered != int64(len(content)) {
		t.Errorf("chunks cover %d bytes, file is %d bytes", covered, len(content))
	}
	if len(sizes) < 2 {
		t.Errorf("all %d chunks have the same size; expected content-defined boundaries", len(chunks))
	}
}

func TestZstdChunkedDeterministic(t *testing.T) {
	t.Parallel()

	input, _ := chunkedTestTar(t)
	build := func() []byte {
		var blob bytes.Buffer
		appender, err := NewSHA256ZstdChunkedTarAppender(&blob, ContentType("tar"), CompressionLevel(3))
		if err != nil {
			t.Fatalf("creating appender: %v", err)
		}
		if err := appender.AppendTar(bytes.NewReader(input)); err != nil {
			t.Fatalf("appending tar: %v", err)
		}
		if _, err := appender.Finalize(); err != nil {
			t.Fatalf("finalizing: %v", err)
		}
		return blob.Bytes()
	}
	if !bytes.Equal(build(), build()) {
		t.Fatal("two builds of the same input produced different blobs")
	}
}

type tocEntry struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	ChunkOffset int64  `json:"chunkOffset"`
	ChunkSize   int64  `json:"chunkSize"`
	ChunkDigest string `json:"chunkDigest"`
}

type tocManifest struct {
	Entries []tocEntry `json:"entries"`
}

func readZstdChunkedTOC(t *testing.T, blob []byte, position string) tocManifest {
	t.Helper()

	var offset, compressedLen, uncompressedLen, manifestType int
	if _, err := fmt.Sscanf(position, "%d:%d:%d:%d", &offset, &compressedLen, &uncompressedLen, &manifestType); err != nil {
		t.Fatalf("parsing manifest position %q: %v", position, err)
	}
	if manifestType != 1 {
		t.Fatalf("manifest type is %d, want 1", manifestType)
	}
	if offset+compressedLen > len(blob) {
		t.Fatalf("manifest at %d+%d is outside the %d byte blob", offset, compressedLen, len(blob))
	}
	r, err := zstd.NewReader(bytes.NewReader(blob[offset : offset+compressedLen]))
	if err != nil {
		t.Fatalf("opening manifest reader: %v", err)
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decompressing manifest: %v", err)
	}
	if len(raw) != uncompressedLen {
		t.Fatalf("manifest is %d bytes uncompressed, annotation says %d", len(raw), uncompressedLen)
	}
	var toc tocManifest
	if err := json.Unmarshal(raw, &toc); err != nil {
		t.Fatalf("decoding manifest: %v", err)
	}
	return toc
}

func chunkedTestTar(t *testing.T) ([]byte, []byte) {
	t.Helper()

	content := make([]byte, 3<<20)
	rand.New(rand.NewSource(1)).Read(content)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, hdr := range []*tar.Header{
		{Typeflag: tar.TypeDir, Name: "dir/", Mode: 0o755},
		{Typeflag: tar.TypeReg, Name: "data.bin", Mode: 0o644, Size: int64(len(content))},
		{Typeflag: tar.TypeSymlink, Name: "dir/link", Linkname: "../data.bin"},
	} {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing tar header: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(content); err != nil {
				t.Fatalf("writing tar content: %v", err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	return buf.Bytes(), content
}
