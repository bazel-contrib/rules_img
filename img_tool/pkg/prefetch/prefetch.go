// Package prefetch provides a go-containerregistry v1.Layer wrapper that reads
// ahead ("prefetches") a layer's contents into an in-memory buffer on a
// background goroutine.
//
// The wrapped layer behaves exactly like the layer it wraps, except that
// Compressed and Uncompressed return readers that are fed by a background
// goroutine which eagerly pulls data from the underlying reader into a bounded
// ring buffer. A consumer that reads more slowly than the underlying source can
// deliver (for example because it is bottlenecked on a network upload) therefore
// does not stall the source: up to the configured prefetch size (by default 64
// MiB) is kept ready in memory ahead of the consumer's read position.
package prefetch

import (
	"io"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// DefaultSize is the default maximum number of bytes to read ahead of the
// consumer.
const DefaultSize = 64 << 20 // 64 MiB

// Option configures a prefetching layer.
type Option func(*layer)

// WithSize sets the maximum number of bytes to prefetch ahead of the consumer.
// A value <= 0 disables prefetching, in which case NewLayer returns the wrapped
// layer unchanged.
func WithSize(n int) Option {
	return func(l *layer) { l.size = n }
}

// layer wraps a v1.Layer and prefetches its (un)compressed contents. Digest,
// DiffID, Size and MediaType are promoted unchanged from the embedded layer;
// only Compressed and Uncompressed are overridden.
type layer struct {
	v1.Layer
	size int
}

var _ v1.Layer = (*layer)(nil)

// NewLayer returns a v1.Layer that behaves like wrapped, but whose Compressed
// and Uncompressed readers are backed by a background goroutine that prefetches
// up to size bytes (DefaultSize unless WithSize is used) into memory ahead of
// the consumer.
//
// If prefetching is disabled (size <= 0), the wrapped layer is returned
// unchanged so that no goroutine is started and any type assertions callers
// perform on the layer keep working.
func NewLayer(wrapped v1.Layer, opts ...Option) v1.Layer {
	l := &layer{Layer: wrapped, size: DefaultSize}
	for _, opt := range opts {
		opt(l)
	}
	if l.size <= 0 {
		return wrapped
	}
	return l
}

func (l *layer) Compressed() (io.ReadCloser, error) {
	rc, err := l.Layer.Compressed()
	if err != nil {
		return nil, err
	}
	// Size reports the compressed size, so we can right-size the buffer and
	// avoid allocating the full prefetch size for small blobs (configs,
	// manifests, tiny layers).
	hint := int64(-1)
	if sz, err := l.Layer.Size(); err == nil {
		hint = sz
	}
	return NewReadCloser(rc, bufSize(l.size, hint)), nil
}

func (l *layer) Uncompressed() (io.ReadCloser, error) {
	rc, err := l.Layer.Uncompressed()
	if err != nil {
		return nil, err
	}
	// The uncompressed size is not known from the descriptor, so use the full
	// prefetch size; a larger-than-needed buffer is harmless as the ring buffer
	// only ever holds as much as the source produces.
	return NewReadCloser(rc, l.size), nil
}

// bufSize returns the ring-buffer size to use given the configured prefetch size
// and an optional size hint (-1 when unknown). The result is clamped to
// [1, prefetch].
func bufSize(prefetch int, hint int64) int {
	n := prefetch
	if hint >= 0 && hint < int64(prefetch) {
		n = int(hint)
	}
	if n < 1 {
		n = 1
	}
	return n
}

// NewReadCloser wraps src in an io.ReadCloser that prefetches up to
// prefetchBytes of src's contents into an in-memory ring buffer on a background
// goroutine. The background goroutine keeps the buffer full so that a consumer
// which reads slower than src can deliver is not what limits src.
//
// The returned ReadCloser must be closed by the caller. Closing it stops the
// background goroutine and closes src. Because the background goroutine may be
// blocked in src.Read when Close is called, src must tolerate Close being
// called concurrently with an in-flight Read — the standard contract for
// cancellable streaming readers such as *os.File, http.Response.Body and
// io.PipeReader (all of the blob sources this package wraps). If prefetchBytes
// <= 0, src is returned unchanged.
func NewReadCloser(src io.ReadCloser, prefetchBytes int) io.ReadCloser {
	if prefetchBytes <= 0 {
		return src
	}
	r := &prefetchReadCloser{
		src: src,
		buf: make([]byte, prefetchBytes),
	}
	r.cond = sync.NewCond(&r.mu)
	go r.prefetch()
	return r
}

// prefetchReadCloser is a single-producer/single-consumer bounded ring buffer.
// The producer goroutine (prefetch) fills the free region of buf; the consumer
// (Read) drains the occupied region. Only the shared indices are guarded by mu;
// the actual byte copies happen without the lock held, because the producer and
// consumer always touch disjoint regions of buf.
type prefetchReadCloser struct {
	src io.ReadCloser

	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte // ring buffer; length fixed at construction
	r      int    // index of the next byte to hand to the consumer
	n      int    // number of buffered (unread) bytes; occupied region is [r, r+n) mod len(buf)
	err    error  // terminal error from src (io.EOF on a clean end); valid once done is true
	done   bool   // producer has finished and set err
	closed bool   // consumer called Close
}

// prefetch runs on a background goroutine and eagerly fills the buffer from src
// until the buffer is full (then waits for the consumer), src reports an error
// or EOF, or the consumer closes the reader.
func (p *prefetchReadCloser) prefetch() {
	for {
		p.mu.Lock()
		for p.n == len(p.buf) && !p.closed {
			p.cond.Wait()
		}
		if p.closed {
			p.mu.Unlock()
			return
		}
		// Fill the contiguous free span starting at the write index. Anything
		// beyond the end of the buffer (wrap-around) is picked up on the next
		// iteration.
		w := (p.r + p.n) % len(p.buf)
		room := len(p.buf) - p.n
		span := len(p.buf) - w
		if span > room {
			span = room
		}
		dst := p.buf[w : w+span]
		p.mu.Unlock()

		m, err := p.src.Read(dst)

		p.mu.Lock()
		if m > 0 {
			p.n += m
			p.cond.Broadcast()
		}
		if err != nil {
			p.err = err
			p.done = true
			p.cond.Broadcast()
			p.mu.Unlock()
			return
		}
		if p.closed {
			// The consumer closed while this read was in flight (and closed
			// src to interrupt it). Stop rather than spinning on a now-closed
			// source; this also bounds a source that returns (0, nil).
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
	}
}

func (p *prefetchReadCloser) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	p.mu.Lock()
	for p.n == 0 && !p.done && !p.closed {
		p.cond.Wait()
	}
	if p.closed {
		p.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if p.n == 0 {
		// Producer finished and the buffer is drained: surface its terminal
		// error (io.EOF on a clean end).
		err := p.err
		p.mu.Unlock()
		return 0, err
	}
	// Hand back the contiguous occupied span starting at the read index.
	r := p.r
	span := len(p.buf) - r
	if span > p.n {
		span = p.n
	}
	if span > len(b) {
		span = len(b)
	}
	src := p.buf[r : r+span]
	p.mu.Unlock()

	copy(b, src)

	p.mu.Lock()
	p.r = (p.r + span) % len(p.buf)
	p.n -= span
	p.cond.Broadcast()
	p.mu.Unlock()
	return span, nil
}

func (p *prefetchReadCloser) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
	// Closing src unblocks the producer if it is currently blocked in
	// src.Read (the standard way to cancel a streaming read); if it is parked
	// in cond.Wait it has already been woken by the broadcast above and will
	// observe closed. This is why src must tolerate Close being called
	// concurrently with an in-flight Read (see NewReadCloser).
	return p.src.Close()
}
