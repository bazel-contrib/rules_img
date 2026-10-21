package compress

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// TestEstargzContentHashMatchesBlob checks that the content hash and the
// uncompressed size of an estargz layer describe the tar stream the estargz
// writer produced, and not the tar stream that was appended.
//
// The estargz writer re-encodes every entry and appends its own
// stargz.index.json entry, so a tar that does not already use the exact
// encoding of archive/tar comes back out as different bytes. Deriving the
// diff_id from the input would then name a stream that nothing can produce
// from the layer blob.
func TestEstargzContentHashMatchesBlob(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		newTar     func(w io.Writer, opts ...Option) (*TarAppender[*EstargzWriter], error)
		decompress func(t *testing.T, blob []byte) []byte
	}{
		{
			name:       "gzip",
			newTar:     NewSHA256EstargzGzipTarAppender,
			decompress: gunzip,
		},
		{
			name:       "zstd",
			newTar:     NewSHA256EstargzZstdTarAppender,
			decompress: unzstd,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var blob bytes.Buffer
			appender, err := tc.newTar(&blob)
			if err != nil {
				t.Fatalf("creating appender: %v", err)
			}
			if err := appender.AppendTar(bytes.NewReader(nonCanonicalTar(t))); err != nil {
				t.Fatalf("appending tar: %v", err)
			}
			state, err := appender.Finalize()
			if err != nil {
				t.Fatalf("finalizing: %v", err)
			}

			uncompressed := tc.decompress(t, blob.Bytes())
			if want := sha256.Sum256(uncompressed); !bytes.Equal(state.ContentHash, want[:]) {
				t.Errorf("content hash is %x, but the uncompressed blob hashes to %x", state.ContentHash, want)
			}
			if got, want := state.UncompressedSize, int64(len(uncompressed)); got != want {
				t.Errorf("uncompressed size is %d, but the uncompressed blob is %d bytes", got, want)
			}
			if got, want := state.LayerAnnotations[api.UncompressedSizeAnnotation], "4096"; got == want {
				t.Errorf("uncompressed size annotation is the size of the input tar (%s), not of the written stream", got)
			}
		})
	}
}

// nonCanonicalTar returns a tar whose encoding differs from what archive/tar
// writes: it is padded out to a full 4096 byte record, the way GNU tar pads by
// default, while archive/tar only writes the two zero blocks that end the
// archive.
func nonCanonicalTar(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte("hello estargz\n")
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     "hello.txt",
		Mode:     0o644,
		Size:     int64(len(content)),
	}); err != nil {
		t.Fatalf("writing tar header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("writing tar content: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	padded := make([]byte, 4096)
	copy(padded, buf.Bytes())
	return padded
}

func gunzip(t *testing.T, blob []byte) []byte {
	t.Helper()

	// An estargz blob is a concatenation of gzip streams: the entries, the
	// stargz.index.json entry and an empty footer stream. gzip.Reader reads
	// all of them by default, which is the full uncompressed tar stream.
	r, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("opening gzip reader: %v", err)
	}
	defer r.Close()
	uncompressed, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decompressing blob: %v", err)
	}
	return uncompressed
}

func unzstd(t *testing.T, blob []byte) []byte {
	t.Helper()

	// The zstd:chunked TOC and footer live in skippable frames, which the
	// decoder drops, so this is the tar stream alone.
	r, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("opening zstd reader: %v", err)
	}
	defer r.Close()
	uncompressed, err := r.DecodeAll(blob, nil)
	if err != nil {
		t.Fatalf("decompressing blob: %v", err)
	}
	return uncompressed
}
