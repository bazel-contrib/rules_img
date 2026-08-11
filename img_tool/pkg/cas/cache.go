package cas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultCacheBufferSize is how much of a blob is written to disk before it
	// is published to readers.
	DefaultCacheBufferSize = 1 << 20 // 1 MiB

	// DefaultCacheMaxSize is a reasonable size limit for a cache directory that
	// nothing else manages. It is not applied automatically: an unset limit means
	// unlimited, which is what a directory shared with Bazel wants (Bazel's disk
	// cache GC owns it).
	DefaultCacheMaxSize = 10 << 30 // 10 GiB

	// cacheDirName is the directory created inside the user cache directory when
	// no cache directory is configured.
	cacheDirName = "rules_img"

	// closeWaitTimeout bounds how long Close waits for the fetch goroutines to
	// notice its cancellation and let go of their files.
	closeWaitTimeout = 5 * time.Second

	// maxAttachAttempts bounds how often ReaderForBlob retries the
	// cache-hit/join-a-fetch dance before it gives up and reads straight from
	// upstream. Retries only happen when a fetch completes or is abandoned in the
	// window between looking it up and attaching to it.
	maxAttachAttempts = 3
)

// CachingReader is a BlobSource that deduplicates reads and caches blobs on
// disk. It wraps another BlobSource (typically a *Pool) and adds two things:
//
//   - Request deduplication. Concurrent reads of one digest share a single
//     upstream fetch; a caller arriving while a fetch is in flight attaches to it
//     and streams the bytes as they arrive rather than opening a second read.
//   - Local caching. Fetched blobs are written into a directory with the same
//     layout as Bazel's disk cache (cas/<first two hex chars>/<hex>), so later
//     reads -- in this process or a later one, and including Bazel itself when
//     the directory is its disk cache -- are served from disk.
//
// Consumers do not wait for a blob to be complete: a fetch publishes what it has
// written every time the configured buffer size fills up, and readers are woken
// as soon as data is available.
//
// Local problems never fail a read. If the cache directory cannot be written,
// runs out of space, or loses a file underneath a reader, the affected read
// degrades to streaming straight from upstream -- blobs are immutable and
// content-addressed, so re-reading and skipping to the reader's offset is always
// correct. Only errors from upstream (and bad content) reach callers.
//
// A CachingReader is safe for concurrent use.
type CachingReader struct {
	upstream   BlobSource
	store      *diskStore
	bufferSize int

	// baseCtx scopes the fetch goroutines. They must outlive the caller that
	// started them, because other callers may be reading the same fetch; Close
	// cancels them all.
	baseCtx context.Context
	cancel  context.CancelFunc

	mu       sync.Mutex
	inflight map[string]*blobFetch

	// running tracks the fetch goroutines, so Close can wait for them to let go
	// of their files before removing anything.
	running sync.WaitGroup

	counters cacheCounters
}

var _ BlobSource = (*CachingReader)(nil)

type cacheCounters struct {
	hits           atomic.Uint64
	fetches        atomic.Uint64
	deduped        atomic.Uint64
	fallbacks      atomic.Uint64
	bytesFromCache atomic.Int64
	bytesFetched   atomic.Int64
}

// CacheStats is a snapshot of a CachingReader's counters.
type CacheStats struct {
	Hits           uint64 // blobs served from the cache directory
	Fetches        uint64 // blobs fetched from upstream
	Deduped        uint64 // reads that joined a fetch already in flight
	Fallbacks      uint64 // reads streamed from upstream without caching
	BytesFromCache int64
	BytesFetched   int64
	Evicted        uint64 // blobs removed from the cache directory
	DiskDisabled   bool   // disk caching gave up (see diskStore.disable)
}

// CacheOption configures a CachingReader.
type CacheOption func(*cacheOptions)

type cacheOptions struct {
	dir        string
	maxSize    int64
	bufferSize int
}

// WithCacheDir sets the cache directory. It is used as the root of a
// Bazel-disk-cache layout, so passing Bazel's --disk_cache directory shares the
// cache with Bazel. The empty default means <user cache dir>/rules_img, or a
// temporary directory (removed by Close) if there is no user cache directory.
func WithCacheDir(dir string) CacheOption {
	return func(o *cacheOptions) { o.dir = dir }
}

// WithCacheMaxSize limits how many bytes of cached blobs to keep, evicting the
// least recently used ones beyond that. Zero (the default) means unlimited: the
// cache directory is then neither indexed at startup nor pruned, and only an
// out-of-space error evicts anything.
func WithCacheMaxSize(n int64) CacheOption {
	return func(o *cacheOptions) { o.maxSize = n }
}

// WithCacheBufferSize sets how much of a blob is written to disk before it is
// published to readers. Defaults to DefaultCacheBufferSize.
func WithCacheBufferSize(n int) CacheOption {
	return func(o *cacheOptions) { o.bufferSize = n }
}

// NewCachingReader returns a CachingReader in front of upstream. It fails only
// if the cache directory cannot be prepared; callers that treat caching as
// optional can then fall back to using upstream directly.
func NewCachingReader(upstream BlobSource, opts ...CacheOption) (*CachingReader, error) {
	if upstream == nil {
		return nil, errors.New("cas: upstream blob source is required")
	}
	options := cacheOptions{bufferSize: DefaultCacheBufferSize}
	for _, opt := range opts {
		opt(&options)
	}
	if options.bufferSize <= 0 {
		options.bufferSize = DefaultCacheBufferSize
	}

	dir, ephemeral, err := resolveCacheDir(options.dir)
	if err != nil {
		return nil, err
	}
	store, err := newDiskStore(dir, ephemeral, options.maxSize)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &CachingReader{
		upstream:   upstream,
		store:      store,
		bufferSize: options.bufferSize,
		baseCtx:    ctx,
		cancel:     cancel,
		inflight:   make(map[string]*blobFetch),
	}, nil
}

// resolveCacheDir picks the cache directory, reporting whether it is a
// temporary one that Close should remove.
func resolveCacheDir(dir string) (string, bool, error) {
	if dir != "" {
		return dir, false, nil
	}
	if userCacheDir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(userCacheDir, cacheDirName), false, nil
	}
	// No user cache directory (no HOME, for example). A temporary directory still
	// deduplicates reads within this process.
	tempDir, err := os.MkdirTemp("", "img-cas-cache-")
	if err != nil {
		return "", false, fmt.Errorf("creating temporary CAS cache directory: %w", err)
	}
	return tempDir, true, nil
}

// Dir returns the cache directory.
func (c *CachingReader) Dir() string {
	return c.store.dir
}

// Stats returns a snapshot of the cache counters.
func (c *CachingReader) Stats() CacheStats {
	return CacheStats{
		Hits:           c.counters.hits.Load(),
		Fetches:        c.counters.fetches.Load(),
		Deduped:        c.counters.deduped.Load(),
		Fallbacks:      c.counters.fallbacks.Load(),
		BytesFromCache: c.counters.bytesFromCache.Load(),
		BytesFetched:   c.counters.bytesFetched.Load(),
		Evicted:        c.store.evictions(),
		DiskDisabled:   c.store.isDisabled(),
	}
}

// Close cancels every fetch still in flight, waits for them to stop writing,
// cleans up their partial files and removes the cache directory if it was a
// temporary one. Cached blobs in a configured directory are left for the next
// run.
func (c *CachingReader) Close() error {
	c.cancel()
	// Wait for the fetch goroutines to close their files: on Windows a file with
	// an open handle can be neither removed nor renamed. A source that ignores
	// cancellation must not keep the process from exiting, so the wait is bounded.
	c.waitForFetches()

	c.mu.Lock()
	fetches := make([]*blobFetch, 0, len(c.inflight))
	for _, fetch := range c.inflight {
		fetches = append(fetches, fetch)
	}
	c.inflight = make(map[string]*blobFetch)
	c.mu.Unlock()

	for _, fetch := range fetches {
		os.Remove(fetch.tempPath)
	}
	return c.store.close()
}

// waitForFetches waits for the fetch goroutines to return, up to
// closeWaitTimeout.
func (c *CachingReader) waitForFetches() {
	stopped := make(chan struct{})
	go func() {
		c.running.Wait()
		close(stopped)
	}()
	timer := time.NewTimer(closeWaitTimeout)
	defer timer.Stop()
	select {
	case <-stopped:
	case <-timer.C:
	}
}

// FindMissingBlobs answers from the cache directory where it can: a blob that is
// cached locally is readable regardless of what upstream still has, so it is
// neither asked about nor reported missing.
func (c *CachingReader) FindMissingBlobs(ctx context.Context, digests []Digest) ([]Digest, error) {
	if len(digests) == 0 {
		return nil, nil
	}
	remaining := make([]Digest, 0, len(digests))
	for _, digest := range digests {
		if digest.SizeBytes > 0 && c.store.has(digest) {
			continue
		}
		remaining = append(remaining, digest)
	}
	if len(remaining) == 0 {
		return nil, nil
	}
	return c.upstream.FindMissingBlobs(ctx, remaining)
}

// ReadBlob returns the whole blob, going through the same cache and
// deduplication as ReaderForBlob.
func (c *CachingReader) ReadBlob(ctx context.Context, digest Digest) ([]byte, error) {
	if digest.SizeBytes == 0 {
		return c.upstream.ReadBlob(ctx, digest)
	}
	reader, err := c.ReaderForBlob(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	buf := make([]byte, digest.SizeBytes)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return nil, fmt.Errorf("failed to read blob: %w", err)
	}
	return buf, nil
}

// ReaderForBlob returns a reader for the blob, served from the cache directory
// if it is there, otherwise from a single shared fetch of the blob that also
// populates the cache.
func (c *CachingReader) ReaderForBlob(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	if digest.SizeBytes == 0 {
		return c.upstream.ReaderForBlob(ctx, digest)
	}

	for range maxAttachAttempts {
		if file, err := c.store.open(digest); err == nil {
			c.counters.hits.Add(1)
			c.counters.bytesFromCache.Add(digest.SizeBytes)
			return file, nil
		}

		fetch, joined := c.fetchFor(digest)
		if fetch == nil {
			break // the cache directory is unusable; read straight from upstream
		}
		reader, err := fetch.attach(ctx)
		if err == nil {
			if joined {
				c.counters.deduped.Add(1)
			}
			return reader, nil
		}
		// The fetch completed or was abandoned between the lookup and the attach.
		// Look again: the blob is probably in the cache directory now.
	}
	return c.passthrough(ctx, digest, 0)
}

// fetchFor returns the fetch for a digest, starting one if none is in flight,
// and reports whether it joined an existing one. It returns nil if the blob
// cannot be cached locally.
func (c *CachingReader) fetchFor(digest Digest) (*blobFetch, bool) {
	key := digest.cacheKey()

	c.mu.Lock()
	defer c.mu.Unlock()
	if fetch, ok := c.inflight[key]; ok {
		return fetch, true
	}
	fetch := c.startFetch(digest, key)
	if fetch == nil {
		return nil, false
	}
	c.inflight[key] = fetch
	return fetch, false
}

// startFetch begins fetching a blob into a temp file. c.mu must be held.
func (c *CachingReader) startFetch(digest Digest, key string) *blobFetch {
	file, err := c.store.createTemp(digest.SizeBytes)
	if err != nil {
		if !errors.Is(err, errDiskCacheDisabled) {
			// The cache directory is not writable at all; stop trying.
			c.store.disable(err)
		}
		return nil
	}
	ctx, cancel := context.WithCancel(c.baseCtx)
	fetch := &blobFetch{
		cache:    c,
		digest:   digest,
		key:      key,
		tempPath: file.Name(),
		cancel:   cancel,
		changed:  make(chan struct{}),
	}
	c.counters.fetches.Add(1)
	c.running.Add(1)
	go fetch.run(ctx, file)
	return fetch
}

// forget drops a fetch from the in-flight map unless it has already been
// replaced by a newer one for the same digest.
func (c *CachingReader) forget(key string, fetch *blobFetch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[key] == fetch {
		delete(c.inflight, key)
	}
}

// passthrough reads a blob straight from upstream, skipping the first off bytes.
// It is how every local failure degrades.
func (c *CachingReader) passthrough(ctx context.Context, digest Digest, off int64) (io.ReadCloser, error) {
	c.counters.fallbacks.Add(1)
	reader, err := c.upstream.ReaderForBlob(ctx, digest)
	if err != nil {
		return nil, err
	}
	if off > 0 {
		if _, err := io.CopyN(io.Discard, reader, off); err != nil {
			reader.Close()
			return nil, fmt.Errorf("skipping %d bytes of blob %s: %w", off, digest.hexHash(), err)
		}
	}
	return reader, nil
}

// blobFetch is one in-flight download of a blob into the cache directory, shared
// by every reader that wants it.
type blobFetch struct {
	cache    *CachingReader
	digest   Digest
	key      string
	tempPath string
	cancel   context.CancelFunc

	mu sync.Mutex
	// changed is closed and replaced whenever published or the terminal state
	// changes, so readers can wait on it while still honoring their context.
	changed   chan struct{}
	published int64 // bytes written to tempPath and visible to readers
	readers   int
	done      bool
	err       error
	local     bool // err is a local problem: readers should fall back to upstream
	abandoned bool // no readers left before completion; no longer attachable
	finalized bool
	discarded bool
}

// fetchState is a snapshot of what a reader needs to decide what to do next.
type fetchState struct {
	available int64
	done      bool
	local     bool
	err       error
}

func (b *blobFetch) run(ctx context.Context, file *os.File) {
	defer b.cache.running.Done()
	err := b.stream(ctx, file)
	file.Close()
	b.finish(err)
}

// stream copies the blob from upstream into file, publishing every full buffer,
// and verifies size and content digest at the end.
func (b *blobFetch) stream(ctx context.Context, file *os.File) error {
	source, err := b.cache.upstream.ReaderForBlob(ctx, b.digest)
	if err != nil {
		return err
	}
	defer source.Close()

	hasher := digestHasher(b.digest)
	// Read one byte past the expected size: enough to notice a source that sends
	// too much, without letting it write an unbounded amount to disk.
	limited := io.LimitReader(source, b.digest.SizeBytes+1)
	buf := make([]byte, b.cache.bufferSize)
	var total int64
	for {
		n, readErr := io.ReadFull(limited, buf)
		if n > 0 {
			if err := b.cache.store.writeAll(file, buf[:n]); err != nil {
				b.cache.store.disable(err)
				return &localCacheError{err: err}
			}
			if hasher != nil {
				hasher.Write(buf[:n])
			}
			total += int64(n)
			b.publish(total)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				break
			}
			return readErr
		}
	}

	if total != b.digest.SizeBytes {
		return fmt.Errorf("blob %s: read %d bytes from remote cache, expected %d", b.digest.hexHash(), total, b.digest.SizeBytes)
	}
	if hasher != nil {
		if sum := hasher.Sum(nil); !bytes.Equal(sum, b.digest.Hash) {
			return fmt.Errorf("blob %s: content from remote cache hashes to %x", b.digest.hexHash(), sum)
		}
	}
	return nil
}

// publish makes the first total bytes of the temp file visible to readers.
func (b *blobFetch) publish(total int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = total
	b.broadcastLocked()
}

// broadcastLocked wakes every reader waiting for a state change.
func (b *blobFetch) broadcastLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// finish records the terminal state of the fetch and either caches the blob or
// cleans up after it.
func (b *blobFetch) finish(err error) {
	var localErr *localCacheError
	isLocal := errors.As(err, &localErr)
	if isLocal {
		err = localErr.err
	}

	b.mu.Lock()
	b.done = true
	b.err = err
	b.local = isLocal
	readers := b.readers
	b.broadcastLocked()
	b.mu.Unlock()

	if err != nil {
		// Let the next caller try again, and drop the partial file once nobody
		// holds it open any more: readers still attached keep their handle (and,
		// for a local failure, fall back to upstream), and on Windows a file with
		// an open handle cannot be removed at all.
		b.cache.forget(b.key, b)
		if readers == 0 {
			b.discard()
		}
		return
	}
	b.cache.counters.bytesFetched.Add(b.digest.SizeBytes)
	if readers == 0 {
		b.finalize()
	}
}

// discard removes the partial file of a fetch that failed. It runs once, when
// the fetch and every reader of it are done with the file.
func (b *blobFetch) discard() {
	b.mu.Lock()
	first := !b.discarded
	b.discarded = true
	b.mu.Unlock()
	if !first {
		return
	}
	b.cache.store.discard(b.tempPath, b.digest.SizeBytes)
}

// attach returns a reader over the fetch's temp file. It fails if the file
// cannot be opened or the fetch has been abandoned, which makes the caller look
// for the blob again.
func (b *blobFetch) attach(ctx context.Context) (io.ReadCloser, error) {
	b.mu.Lock()
	if b.abandoned {
		b.mu.Unlock()
		return nil, errors.New("cas: blob fetch abandoned")
	}
	b.readers++
	b.mu.Unlock()

	file, err := os.Open(b.tempPath)
	if err != nil {
		b.detach()
		return nil, err
	}
	return &fetchReader{fetch: b, ctx: ctx, file: file}, nil
}

// detach releases a reader. The last reader to leave either publishes the blob
// into the cache directory or, if the blob is still incomplete, cancels the
// fetch.
func (b *blobFetch) detach() {
	b.mu.Lock()
	b.readers--
	readers, done, err := b.readers, b.done, b.err
	// A fetch that has already written every byte is worth finishing even with
	// nobody reading it: it only has verification left, and a reader that
	// consumed the whole blob routinely closes before the fetch records that it
	// is done.
	abandon := readers <= 0 && !done && b.published < b.digest.SizeBytes
	if abandon {
		b.abandoned = true
	}
	b.mu.Unlock()

	if readers > 0 {
		return
	}
	if abandon {
		// Nobody wants this blob any more. The fetch goroutine notices the
		// cancellation and removes the temp file.
		b.cache.forget(b.key, b)
		b.cancel()
		return
	}
	if done && err == nil {
		b.finalize()
		return
	}
	if done {
		// The fetch failed and this was the last handle on its partial file.
		b.discard()
	}
}

// finalize moves the completed blob into the cache directory. A failure here
// only costs the caching of one blob, so it is not reported.
func (b *blobFetch) finalize() {
	b.mu.Lock()
	eligible := b.done && b.err == nil && b.readers == 0 && !b.finalized
	if eligible {
		b.finalized = true
	}
	b.mu.Unlock()
	if !eligible {
		return
	}
	b.cache.forget(b.key, b)
	if err := b.cache.store.finalize(b.tempPath, b.digest); err != nil && isNoSpace(err) {
		b.cache.store.disable(err)
	}
}

// waitFor blocks until data past off is available or the fetch has ended,
// honoring the reader's context.
func (b *blobFetch) waitFor(ctx context.Context, off int64) (fetchState, error) {
	for {
		b.mu.Lock()
		state := fetchState{available: b.published, done: b.done, local: b.local, err: b.err}
		changed := b.changed
		b.mu.Unlock()

		if state.available > off || state.done {
			return state, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return fetchState{}, ctx.Err()
		}
	}
}

// fetchReader streams one blob from a shared fetch, reading the temp file as it
// grows and falling back to upstream if the local copy fails.
type fetchReader struct {
	fetch *blobFetch
	ctx   context.Context

	file        *os.File
	passthrough io.ReadCloser
	off         int64
	detached    bool
	closed      bool
}

func (r *fetchReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if r.passthrough != nil {
			n, err := r.passthrough.Read(p)
			r.off += int64(n)
			return n, err
		}

		state, err := r.fetch.waitFor(r.ctx, r.off)
		if err != nil {
			return 0, err
		}
		if state.available > r.off {
			limit := min(int64(len(p)), state.available-r.off)
			n, readErr := r.file.Read(p[:limit])
			r.off += int64(n)
			if n > 0 {
				return n, nil
			}
			// Published bytes must be readable; if they are not, the local file is
			// broken and upstream is the only way forward.
			if fallbackErr := r.startPassthrough(); fallbackErr != nil {
				return 0, fmt.Errorf("reading cached blob %s: %w", r.fetch.digest.hexHash(), errors.Join(readErr, fallbackErr))
			}
			continue
		}
		// The fetch has ended.
		if state.err == nil {
			return 0, io.EOF
		}
		if !state.local {
			return 0, state.err
		}
		if fallbackErr := r.startPassthrough(); fallbackErr != nil {
			return 0, errors.Join(state.err, fallbackErr)
		}
	}
}

// startPassthrough switches this reader over to a fresh upstream read, skipping
// the bytes it already delivered.
func (r *fetchReader) startPassthrough() error {
	reader, err := r.fetch.cache.passthrough(r.ctx, r.fetch.digest, r.off)
	if err != nil {
		return err
	}
	if r.file != nil {
		r.file.Close()
		r.file = nil
	}
	r.passthrough = reader
	// This reader no longer depends on the fetch.
	if !r.detached {
		r.detached = true
		r.fetch.detach()
	}
	return nil
}

func (r *fetchReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true

	var errs []error
	if r.file != nil {
		errs = append(errs, r.file.Close())
		r.file = nil
	}
	if r.passthrough != nil {
		errs = append(errs, r.passthrough.Close())
		r.passthrough = nil
	}
	if !r.detached {
		r.detached = true
		r.fetch.detach()
	}
	return errors.Join(errs...)
}

// localCacheError marks an error as a local cache failure, which readers
// recover from by reading straight from upstream.
type localCacheError struct {
	err error
}

func (e *localCacheError) Error() string { return e.err.Error() }
func (e *localCacheError) Unwrap() error { return e.err }

// digestHasher returns a hasher for the digest's algorithm, or nil if we cannot
// verify content for it.
func digestHasher(digest Digest) hash.Hash {
	switch digest.algorithm {
	case "sha256":
		return sha256.New()
	case "sha512":
		return sha512.New()
	}
	return nil
}

// hexHash returns the digest's hash in hex, which is also its file name in the
// cache directory.
func (d Digest) hexHash() string {
	return hex.EncodeToString(d.Hash)
}

// cacheKey identifies a blob for in-flight deduplication.
func (d Digest) cacheKey() string {
	return d.algorithm + ":" + d.hexHash()
}
