package tarcas

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"io"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
)

// recordingReadSeeker reports how a reader was consumed, which is what
// separates streaming (hash, rewind, write) from buffering (one read, content
// retained in memory).
type recordingReadSeeker struct {
	*bytes.Reader
	seeks int
}

func (r *recordingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	r.seeks++
	return r.Reader.Seek(offset, whence)
}

// readerOnly hides an underlying Seek method, standing in for content that
// genuinely cannot be rewound.
type readerOnly struct{ r io.Reader }

func (r readerOnly) Read(p []byte) (int, error) { return r.r.Read(p) }

func TestHashRewindableStreamsSeekableReader(t *testing.T) {
	content := bytes.Repeat([]byte("tree artifact blob "), 1024)
	rs := &recordingReadSeeker{Reader: bytes.NewReader(content)}

	h := sha256.New()
	n, rewound, err := hashRewindable(h, rs)
	if err != nil {
		t.Fatal(err)
	}
	if !rewound {
		t.Fatal("seekable reader was not rewound, so its content would be buffered")
	}
	if n != int64(len(content)) {
		t.Errorf("size = %d, want %d", n, len(content))
	}
	want := sha256.Sum256(content)
	if got := h.Sum(nil); !bytes.Equal(got, want[:]) {
		t.Errorf("hash = %x, want %x", got, want)
	}

	// The whole point is that the content is still readable afterwards: the
	// caller streams it a second time instead of holding a copy.
	rest, err := io.ReadAll(rs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, content) {
		t.Errorf("reader was left at offset %d bytes from the start", len(content)-len(rest))
	}
}

func TestHashRewindableFallsBackForNonSeekableReader(t *testing.T) {
	content := []byte("not rewindable")
	r := readerOnly{r: bytes.NewReader(content)}

	h := sha256.New()
	n, rewound, err := hashRewindable(h, r)
	if err != nil {
		t.Fatal(err)
	}
	if rewound {
		t.Fatal("non-seekable reader reported as rewound")
	}
	if n != 0 {
		t.Errorf("size = %d, want 0: the reader must be left untouched for the caller to buffer", n)
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, content) {
		t.Error("reader was partially consumed, so the buffered fallback would lose bytes")
	}
}

// TestStoreSeekableMatchesBuffered pins the two paths to the same output: a
// seekable reader must produce the byte-identical tar and blob hash that the
// buffering path produces.
func TestStoreSeekableMatchesBuffered(t *testing.T) {
	content := bytes.Repeat([]byte("payload"), 4096)

	store := func(r io.Reader) (string, []byte, []byte) {
		t.Helper()
		var tarBuf bytes.Buffer
		// Uncompressed so the buffer is a plain tar the test can walk.
		appender, err := compress.TarAppenderFactory("sha256", "none", false, &tarBuf)
		if err != nil {
			t.Fatal(err)
		}
		c := New[SHA256Helper](appender)
		path, hash, size, err := c.Store(r, "blob")
		if err != nil {
			t.Fatal(err)
		}
		if size != int64(len(content)) {
			t.Fatalf("size = %d, want %d", size, len(content))
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		return path, hash, tarBuf.Bytes()
	}

	seekPath, seekHash, seekTar := store(bytes.NewReader(content))
	bufPath, bufHash, bufTar := store(readerOnly{r: bytes.NewReader(content)})

	if seekPath != bufPath {
		t.Errorf("path = %q (seekable) vs %q (buffered)", seekPath, bufPath)
	}
	if !bytes.Equal(seekHash, bufHash) {
		t.Errorf("hash = %x (seekable) vs %x (buffered)", seekHash, bufHash)
	}
	if !bytes.Equal(seekTar, bufTar) {
		t.Error("seekable and buffered paths produced different tars")
	}
	var hdrCount int
	tr := tar.NewReader(bytes.NewReader(seekTar))
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		hdrCount++
	}
	if hdrCount == 0 {
		t.Error("no tar entries written")
	}
}
