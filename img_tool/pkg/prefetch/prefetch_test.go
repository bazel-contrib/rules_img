package prefetch

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// blockingReader yields data slowly so a test can observe that the background
// goroutine has prefetched ahead of the consumer.
type blockingReader struct {
	data     []byte
	pos      int
	read     atomic.Int64 // total bytes handed to the prefetcher so far
	gate     chan struct{}
	closed   atomic.Bool
	closeErr error
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if r.gate != nil {
		<-r.gate
	}
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	r.read.Add(int64(n))
	return n, nil
}

func (r *blockingReader) Close() error {
	r.closed.Store(true)
	return r.closeErr
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return b
}

// TestReadCloserRoundTrip verifies the prefetching reader returns exactly the
// bytes of the source across a range of buffer/data-size combinations, including
// ones that force ring-buffer wrap-around and partial reads.
func TestReadCloserRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 7, 1024, 1<<16 + 3}
	bufSizes := []int{1, 3, 64, 4096, 1 << 20}
	for _, dataSize := range sizes {
		for _, bs := range bufSizes {
			data := randomBytes(t, dataSize)
			src := &blockingReader{data: data}
			rc := NewReadCloser(src, bs)
			// Read with a small buffer to exercise partial reads and wrap-around.
			got, err := io.ReadAll(&smallReader{r: rc, chunk: 5})
			if err != nil {
				t.Fatalf("dataSize=%d bufSize=%d: ReadAll: %v", dataSize, bs, err)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("dataSize=%d bufSize=%d: content mismatch (got %d bytes)", dataSize, bs, len(got))
			}
			if err := rc.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if !src.closed.Load() {
				t.Fatalf("dataSize=%d bufSize=%d: underlying source not closed", dataSize, bs)
			}
		}
	}
}

// smallReader forces Read to be called with a small buffer to exercise partial
// reads out of the ring buffer.
type smallReader struct {
	r     io.Reader
	chunk int
}

func (s *smallReader) Read(p []byte) (int, error) {
	if len(p) > s.chunk {
		p = p[:s.chunk]
	}
	return s.r.Read(p)
}

// TestPrefetchRunsAhead verifies the background goroutine reads ahead of the
// consumer: after opening the reader (and reading nothing), the prefetcher pulls
// up to the buffer size from the source on its own.
func TestPrefetchRunsAhead(t *testing.T) {
	const dataSize = 1 << 20
	const bufSize = 64 << 10
	data := randomBytes(t, dataSize)
	src := &blockingReader{data: data, gate: make(chan struct{})}
	// Open the gate freely; the prefetcher will fill the buffer and then block.
	close(src.gate)

	rc := NewReadCloser(src, bufSize)
	defer rc.Close()

	// Without reading anything, the prefetcher should fill the buffer.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if src.read.Load() >= int64(bufSize) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := src.read.Load(); got < int64(bufSize) {
		t.Fatalf("prefetcher did not read ahead: read %d, want >= %d", got, bufSize)
	}

	// The full contents must still come through intact.
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("content mismatch")
	}
}

// errReader returns some data then a non-EOF error.
type errReader struct {
	data []byte
	pos  int
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if r.pos < len(r.data) {
		n := copy(p, r.data[r.pos:])
		r.pos += n
		return n, nil
	}
	return 0, r.err
}

func (r *errReader) Close() error { return nil }

// TestReadPropagatesError verifies a non-EOF error from the source is surfaced
// to the consumer after the buffered prefix has been drained.
func TestReadPropagatesError(t *testing.T) {
	sentinel := errors.New("boom")
	data := randomBytes(t, 100)
	src := &errReader{data: data, err: sentinel}
	rc := NewReadCloser(src, 4096)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("expected buffered prefix to be delivered before the error")
	}
}

// TestCloseUnblocksConsumer verifies Close wakes a consumer blocked waiting for
// data and unblocks a producer stuck in a slow source Read.
func TestCloseUnblocksConsumer(t *testing.T) {
	src := &blockingReader{data: randomBytes(t, 1<<20), gate: make(chan struct{})} // never opened
	rc := NewReadCloser(src, 4096)

	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 16)
		_, err := rc.Read(buf)
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("expected ErrClosedPipe after Close, got %v", err)
		}
	}()

	// Give the consumer a moment to block on the empty buffer.
	time.Sleep(20 * time.Millisecond)
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not return after Close")
	}
}

// TestZeroPrefetchReturnsSource verifies prefetchBytes <= 0 returns the source
// unchanged (no goroutine, no wrapping).
func TestZeroPrefetchReturnsSource(t *testing.T) {
	src := &blockingReader{data: []byte("hello")}
	rc := NewReadCloser(src, 0)
	if rc != io.ReadCloser(src) {
		t.Fatalf("expected source to be returned unchanged when prefetch disabled")
	}
}

// fakeLayer is a minimal v1.Layer whose Compressed/Uncompressed return
// in-memory content, used to test the layer wrapper.
type fakeLayer struct {
	compressed   []byte
	uncompressed []byte
	size         int64
	compOpens    atomic.Int64
}

func (l *fakeLayer) Digest() (v1.Hash, error) { return v1.Hash{Algorithm: "sha256", Hex: "abc"}, nil }
func (l *fakeLayer) DiffID() (v1.Hash, error) { return v1.Hash{Algorithm: "sha256", Hex: "def"}, nil }
func (l *fakeLayer) Size() (int64, error)     { return l.size, nil }
func (l *fakeLayer) MediaType() (types.MediaType, error) {
	return types.OCILayer, nil
}
func (l *fakeLayer) Compressed() (io.ReadCloser, error) {
	l.compOpens.Add(1)
	return io.NopCloser(bytes.NewReader(l.compressed)), nil
}
func (l *fakeLayer) Uncompressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.uncompressed)), nil
}

// TestNewLayer verifies the layer wrapper delivers identical (un)compressed
// content and promotes metadata methods unchanged.
func TestNewLayer(t *testing.T) {
	comp := randomBytes(t, 5000)
	uncomp := randomBytes(t, 9000)
	fl := &fakeLayer{compressed: comp, uncompressed: uncomp, size: int64(len(comp))}

	l := NewLayer(fl)

	rc, err := l.Compressed()
	if err != nil {
		t.Fatalf("Compressed: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, comp) {
		t.Fatalf("compressed mismatch: err=%v", err)
	}

	rc, err = l.Uncompressed()
	if err != nil {
		t.Fatalf("Uncompressed: %v", err)
	}
	got, err = io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, uncomp) {
		t.Fatalf("uncompressed mismatch: err=%v", err)
	}

	// Metadata methods are promoted from the wrapped layer.
	if sz, _ := l.Size(); sz != int64(len(comp)) {
		t.Fatalf("Size mismatch: %d", sz)
	}
	if mt, _ := l.MediaType(); mt != types.OCILayer {
		t.Fatalf("MediaType mismatch: %s", mt)
	}
}

// TestNewLayerDisabled verifies WithSize(0) returns the wrapped layer unchanged
// so type assertions callers rely on keep working.
func TestNewLayerDisabled(t *testing.T) {
	fl := &fakeLayer{compressed: []byte("x"), size: 1}
	l := NewLayer(fl, WithSize(0))
	if l != v1.Layer(fl) {
		t.Fatalf("expected wrapped layer to be returned unchanged when disabled")
	}
}

// TestConcurrentReadClose is a race-detector stress test that opens, reads and
// closes many prefetching readers concurrently.
func TestConcurrentReadClose(t *testing.T) {
	data := randomBytes(t, 200000)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src := &blockingReader{data: data}
			rc := NewReadCloser(src, 4096)
			buf := make([]byte, 777)
			// Read a bit, then close early to exercise mid-stream Close.
			_, _ = rc.Read(buf)
			_, _ = rc.Read(buf)
			_ = rc.Close()
		}()
	}
	wg.Wait()
}

func TestBufSize(t *testing.T) {
	cases := []struct {
		prefetch int
		hint     int64
		want     int
	}{
		{prefetch: 100, hint: -1, want: 100},
		{prefetch: 100, hint: 50, want: 50},
		{prefetch: 100, hint: 200, want: 100},
		{prefetch: 100, hint: 0, want: 1},
		{prefetch: 0, hint: -1, want: 1},
	}
	for _, c := range cases {
		if got := bufSize(c.prefetch, c.hint); got != c.want {
			t.Errorf("bufSize(%d, %d) = %d, want %d", c.prefetch, c.hint, got, c.want)
		}
	}
}
