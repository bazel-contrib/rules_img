package cas

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// EnvPrefetchBytes caps how much prefetched blob data every reader in the
// process holds at once, in bytes. "0" disables reading ahead, which makes a
// blob list fetch one batch at a time.
const EnvPrefetchBytes = "IMG_REAPI_PREFETCH_BYTES"

const (
	// prefetchDepth is how many chunks a reader queues ahead of the one it is
	// delivering. Two is enough to keep a request in flight while the consumer
	// works through the last one, which is what hides the round trip; more only
	// helps when a single batch is too small to fill the link, and the byte
	// budget is what actually bounds it. One more chunk may be in flight while
	// the planner waits for room in the queue.
	prefetchDepth = 2

	// defaultPrefetchBytes is the process-wide ceiling on prefetched data. It is
	// shared rather than per reader on purpose: `img deploy` reconstructs as many
	// layers at once as it has jobs, and a per-reader budget would multiply by
	// the job count on exactly the large machines where that hurts most.
	defaultPrefetchBytes = 64 * 1024 * 1024
)

// prefetchBytes is the environment-derived budget, parsed once: a deploy builds
// many readers and a malformed value is worth one warning.
var prefetchBytes = sync.OnceValue(prefetchBytesFromEnv)

func prefetchBytesFromEnv() int64 {
	raw := strings.TrimSpace(os.Getenv(EnvPrefetchBytes))
	if raw == "" {
		return defaultPrefetchBytes
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || size < 0 {
		fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid %s=%q, using %d\n", EnvPrefetchBytes, raw, defaultPrefetchBytes)
		return defaultPrefetchBytes
	}
	return size
}

// sharedPrefetchBudget is the budget every prefetching reader draws on.
var sharedPrefetchBudget = &prefetchBudget{limit: prefetchBytes}

// prefetchBudget bounds the blob data prefetching readers hold between them.
//
// A reader that holds nothing is admitted whatever the budget says. Without
// that, a reader arriving at a full budget could not fetch the chunk it needs
// next and would wait on readers that are under no obligation to finish -- so
// the guarantee is that every reader can always make progress, and the price is
// an overshoot bounded by one chunk per reader in flight.
type prefetchBudget struct {
	limit func() int64

	mu   sync.Mutex
	used int64
	// changed is closed and replaced whenever bytes are given back, so a waiter
	// can wait on it while still honoring its own cancellation.
	changed chan struct{}
}

// reserve accounts n bytes, waiting for room if the reader already holds some.
// held is the reader's own reservation, read under the lock. It reports false
// if stop or ctx ended the wait.
func (b *prefetchBudget) reserve(ctx context.Context, stop <-chan struct{}, n int64, held *atomic.Int64) bool {
	for {
		b.mu.Lock()
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		if held.Load() == 0 || b.used+n <= b.limit() {
			b.used += n
			held.Add(n)
			b.mu.Unlock()
			return true
		}
		changed := b.changed
		b.mu.Unlock()

		select {
		case <-changed:
		case <-stop:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// release gives n bytes back and wakes everything waiting for room.
func (b *prefetchBudget) release(n int64, held *atomic.Int64) {
	b.mu.Lock()
	held.Add(-n)
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
	if b.changed != nil {
		close(b.changed)
	}
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

// inUse is how many bytes are reserved right now.
func (b *prefetchBudget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// chunk is one step of a blob list: either a run of blobs read into memory
// together, or a single blob streamed on its own because it is too large to
// hold (or because the source can serve it without a fetch).
type chunk struct {
	from, to int
	bytes    int64
	streamed bool
}

// chunkSource is a blob list broken into chunks, which [newPrefetchingReader]
// turns into one stream of the blobs concatenated in order.
//
// plan and fetch run on the reader's own goroutines, several fetches at a time,
// so an implementation must tolerate that. open runs on the consumer's
// goroutine, in list order, and never ahead of time -- which is what keeps a
// streamed chunk from being started before the consumer is ready to read it.
type chunkSource interface {
	// count is how many blobs the list holds.
	count() int
	// plan describes the chunk that starts at index i.
	plan(i int) chunk
	// fetch reads a buffered chunk into memory, one entry per blob.
	fetch(ctx context.Context, c chunk) ([][]byte, error)
	// open starts a streamed chunk.
	open(ctx context.Context, c chunk) (io.ReadCloser, error)
}

// pendingChunk is a chunk on its way to the consumer: either a fetch in flight
// or a streamed chunk waiting to be opened.
type pendingChunk struct {
	chunk chunk
	done  chan struct{} // closed when blobs/err are set; pre-closed if streamed
	blobs [][]byte
	err   error

	// released guards the budget hand-back, which either the consumer (after
	// delivering the chunk) or Close (draining what is left) performs.
	released sync.Once
}

// prefetchingReader delivers a [chunkSource] as one stream, fetching chunks
// ahead of the consumer so a request is in flight while the last one is being
// read.
type prefetchingReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	source chunkSource
	budget *prefetchBudget
	held   atomic.Int64

	queue    chan *pendingChunk
	stop     chan struct{}
	stopOnce sync.Once
	// planErr is why planning stopped early. It is written before queue is
	// closed and read only after the close is observed.
	planErr error

	cur    *pendingChunk
	blobs  [][]byte
	stream io.ReadCloser
	err    error // sticky: the stream delivers nothing after a failure
}

func newPrefetchingReader(ctx context.Context, source chunkSource, budget *prefetchBudget) *prefetchingReader {
	ctx, cancel := context.WithCancel(ctx)
	r := &prefetchingReader{
		ctx:    ctx,
		cancel: cancel,
		source: source,
		budget: budget,
		queue:  make(chan *pendingChunk, prefetchDepth),
		stop:   make(chan struct{}),
	}
	go r.plan()
	return r
}

// plan walks the list, starting each buffered chunk's fetch as it goes and
// queueing the chunks in order for the consumer.
func (r *prefetchingReader) plan() {
	// Always closed, including on an early return: Close drains the queue to
	// give back what was reserved, and a queue left open would hang it.
	defer close(r.queue)

	for i := 0; i < r.source.count(); {
		c := r.source.plan(i)
		pending := &pendingChunk{chunk: c, done: make(chan struct{})}
		if c.streamed {
			close(pending.done)
		} else {
			if !r.budget.reserve(r.ctx, r.stop, c.bytes, &r.held) {
				r.planErr = r.ctx.Err()
				break
			}
			go r.fetch(pending)
		}

		select {
		case r.queue <- pending:
		case <-r.stop:
			r.give(pending)
			return // Close drains and releases whatever else is queued.
		}
		i = c.to
	}
}

// fetch reads one chunk and hands it to whoever is waiting for it.
func (r *prefetchingReader) fetch(pending *pendingChunk) {
	pending.blobs, pending.err = r.source.fetch(r.ctx, pending.chunk)
	close(pending.done)
}

// give returns a chunk's budget, once.
func (r *prefetchingReader) give(pending *pendingChunk) {
	if pending.chunk.streamed {
		return
	}
	pending.released.Do(func() { r.budget.release(pending.chunk.bytes, &r.held) })
}

func (r *prefetchingReader) Read(p []byte) (int, error) {
	for {
		if r.err != nil {
			return 0, r.err
		}
		switch {
		case len(r.blobs) > 0:
			n := copy(p, r.blobs[0])
			r.blobs[0] = r.blobs[0][n:]
			if len(r.blobs[0]) == 0 {
				r.blobs = r.blobs[1:]
			}
			if len(r.blobs) == 0 {
				r.finishChunk()
			}
			return n, nil
		case r.stream != nil:
			n, err := r.stream.Read(p)
			if err == nil {
				return n, nil
			}
			if err != io.EOF {
				r.err = err
				r.closeStream()
				return n, r.err
			}
			if closeErr := r.closeStream(); closeErr != nil {
				r.err = closeErr
			}
			r.finishChunk()
			if n > 0 {
				return n, nil
			}
		default:
			if err := r.advance(); err != nil {
				r.err = err
				return 0, r.err
			}
		}
	}
}

// advance takes the next chunk off the queue and readies it for delivery.
func (r *prefetchingReader) advance() error {
	var pending *pendingChunk
	select {
	case queued, ok := <-r.queue:
		if !ok {
			if r.planErr != nil {
				return r.planErr
			}
			return io.EOF
		}
		pending = queued
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
	// Own it before waiting on it: a wait cut short still has to give the
	// budget back, which Close does through r.cur.
	r.cur = pending

	select {
	case <-pending.done:
	case <-r.ctx.Done():
		return r.ctx.Err()
	}

	if pending.err != nil {
		return pending.err
	}
	if pending.chunk.streamed {
		stream, err := r.source.open(r.ctx, pending.chunk)
		if err != nil {
			return err
		}
		r.stream = stream
		return nil
	}
	if len(pending.blobs) == 0 {
		// Nothing to deliver; hand the budget straight back.
		r.finishChunk()
		return nil
	}
	r.blobs = pending.blobs
	return nil
}

// finishChunk releases the chunk the consumer just finished with.
func (r *prefetchingReader) finishChunk() {
	if r.cur == nil {
		return
	}
	r.give(r.cur)
	r.cur = nil
	r.blobs = nil
}

func (r *prefetchingReader) closeStream() error {
	if r.stream == nil {
		return nil
	}
	err := r.stream.Close()
	r.stream = nil
	return err
}

// Close stops planning, gives back the budget of everything still queued and
// releases the open chunk. Fetches already in flight are cancelled; they are not
// waited for, so their buffers outlive the accounting by as long as it takes
// them to notice -- bounded by the chunks one reader can have in flight.
//
// A read after Close reports EOF rather than the cancellation, which is what a
// reader that had been drained to the end would have said.
func (r *prefetchingReader) Close() error {
	r.stopOnce.Do(func() { close(r.stop) })
	err := r.closeStream()
	r.finishChunk()
	r.cancel()
	for pending := range r.queue {
		r.give(pending)
	}
	if r.err == nil {
		r.err = io.EOF
	}
	return err
}
