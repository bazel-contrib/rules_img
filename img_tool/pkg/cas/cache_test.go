package cas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeBlobs is a BlobSource serving blobs from memory. It counts reads per
// digest and can be told to deliver in fixed-size chunks and to block partway
// through a blob until the test lets it continue.
type fakeBlobs struct {
	// Configured before use and then only read.
	chunk     int           // bytes per Read call (0: as much as fits)
	gate      chan struct{} // if non-nil, block once gateAfter bytes were served
	gateAfter int
	openErr   error // returned by ReaderForBlob instead of a reader

	mu        sync.Mutex
	data      map[string][]byte // hex digest -> served content
	reads     map[string]int
	findCalls [][]Digest
}

func newFakeBlobs(blobs ...[]byte) *fakeBlobs {
	f := &fakeBlobs{data: make(map[string][]byte), reads: make(map[string]int)}
	for _, blob := range blobs {
		f.data[blobDigest(blob).hexHash()] = blob
	}
	return f
}

// serveInstead makes the source answer reads for digest with other content,
// simulating a corrupt or truncated remote blob.
func (f *fakeBlobs) serveInstead(digest Digest, content []byte) {
	f.data[digest.hexHash()] = content
}

func (f *fakeBlobs) FindMissingBlobs(_ context.Context, digests []Digest) ([]Digest, error) {
	f.mu.Lock()
	f.findCalls = append(f.findCalls, slices.Clone(digests))
	f.mu.Unlock()

	var missing []Digest
	for _, digest := range digests {
		f.mu.Lock()
		_, found := f.data[digest.hexHash()]
		f.mu.Unlock()
		if !found {
			missing = append(missing, digest)
		}
	}
	return missing, nil
}

func (f *fakeBlobs) ReadBlob(ctx context.Context, digest Digest) ([]byte, error) {
	reader, err := f.ReaderForBlob(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func (f *fakeBlobs) ReaderForBlob(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	f.mu.Lock()
	f.reads[digest.hexHash()]++
	data, found := f.data[digest.hexHash()]
	f.mu.Unlock()

	if f.openErr != nil {
		return nil, f.openErr
	}
	if !found {
		return nil, fmt.Errorf("fake: blob %s not found", digest.hexHash())
	}
	return &fakeBlobReader{source: f, data: data, ctx: ctx}, nil
}

func (f *fakeBlobs) ReaderForBlobs(ctx context.Context, digests []Digest) (io.ReadCloser, error) {
	var buf bytes.Buffer
	for _, digest := range digests {
		data, err := f.ReadBlob(ctx, digest)
		if err != nil {
			return nil, err
		}
		buf.Write(data)
	}
	return io.NopCloser(&buf), nil
}

func (f *fakeBlobs) readCount(digest Digest) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[digest.hexHash()]
}

func (f *fakeBlobs) findMissingCalls() [][]Digest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.findCalls)
}

type fakeBlobReader struct {
	source *fakeBlobs
	data   []byte
	ctx    context.Context
	pos    int
}

func (r *fakeBlobReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if r.source.gate != nil && r.pos >= r.source.gateAfter {
		select {
		case <-r.source.gate:
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	n := len(p)
	if r.source.chunk > 0 && n > r.source.chunk {
		n = r.source.chunk
	}
	if remaining := len(r.data) - r.pos; n > remaining {
		n = remaining
	}
	// Stop at the gate rather than serving through it.
	if r.source.gate != nil && r.pos < r.source.gateAfter && r.pos+n > r.source.gateAfter {
		n = r.source.gateAfter - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

func (r *fakeBlobReader) Close() error { return nil }

func blobDigest(content []byte) Digest {
	sum := sha256.Sum256(content)
	return SHA256(sum[:], int64(len(content)))
}

// blobContent returns deterministic, distinguishable bytes.
func blobContent(seed byte, size int) []byte {
	content := make([]byte, size)
	for i := range content {
		content[i] = seed + byte(i%251)
	}
	return content
}

func newTestCache(t *testing.T, source BlobSource, opts ...CacheOption) *CachingReader {
	t.Helper()
	opts = append([]CacheOption{WithCacheDir(t.TempDir())}, opts...)
	reader, err := NewCachingReader(source, opts...)
	if err != nil {
		t.Fatalf("NewCachingReader: %v", err)
	}
	t.Cleanup(func() { reader.Close() })
	return reader
}

func readAllAndClose(t *testing.T, reader io.ReadCloser) []byte {
	t.Helper()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading blob: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("closing blob reader: %v", err)
	}
	return data
}

func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// settle waits until no fetch is in flight and no partial blob is left, so that
// what is (or is not) in the cache directory has stopped changing. A reader that
// consumed a whole blob usually closes just before its fetch records completion,
// so caching finishes a moment after the read does. Callers must close their
// readers first: a partial file is only removed once nobody holds it open, which
// on Windows is the only time it can be removed at all.
func settle(t *testing.T, cache *CachingReader) {
	t.Helper()
	eventually(t, "the cache to settle", func() bool {
		entries, err := os.ReadDir(filepath.Join(cache.Dir(), tempDirName))
		if err != nil || len(entries) != 0 {
			return false
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return len(cache.inflight) == 0
	})
}

// assertCached fails unless the blob is in the cache directory with its full size.
func assertCached(t *testing.T, cache *CachingReader, digest Digest) {
	t.Helper()
	path := DiskCacheBlobPath(cache.Dir(), digest.hexHash())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("blob was not cached at %s: %v", path, err)
	}
	if info.Size() != digest.SizeBytes {
		t.Fatalf("cached blob %s has size %d, want %d", path, info.Size(), digest.SizeBytes)
	}
}

// assertNotCached fails if the blob is in the cache directory.
func assertNotCached(t *testing.T, cache *CachingReader, digest Digest) {
	t.Helper()
	path := DiskCacheBlobPath(cache.Dir(), digest.hexHash())
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("blob at %s should not be cached (stat error: %v)", path, err)
	}
}

func TestCachingReaderCachesBlobOnDisk(t *testing.T) {
	content := blobContent(1, 4096)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	cache := newTestCache(t, source)

	reader, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllAndClose(t, reader); !slices.Equal(got, content) {
		t.Fatalf("first read returned %d bytes, want the blob", len(got))
	}
	settle(t, cache)
	assertCached(t, cache, digest)

	// A second read is served from disk without touching upstream.
	reader, err = cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := readAllAndClose(t, reader); !slices.Equal(got, content) {
		t.Fatal("second read returned the wrong bytes")
	}
	if reads := source.readCount(digest); reads != 1 {
		t.Errorf("upstream was read %d times, want 1", reads)
	}
	stats := cache.Stats()
	if stats.Hits != 1 || stats.Fetches != 1 {
		t.Errorf("stats = %+v, want 1 hit and 1 fetch", stats)
	}
}

func TestCachingReaderReadBlobSharesCache(t *testing.T) {
	content := blobContent(2, 1024)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	cache := newTestCache(t, source)

	got, err := cache.ReadBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, content) {
		t.Fatal("ReadBlob returned the wrong bytes")
	}
	settle(t, cache)

	reader, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	readAllAndClose(t, reader)
	if reads := source.readCount(digest); reads != 1 {
		t.Errorf("upstream was read %d times, want 1", reads)
	}
}

func TestCachingReaderDeduplicatesInFlightReads(t *testing.T) {
	content := blobContent(3, 64)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	source.chunk = 16
	source.gateAfter = 16
	source.gate = make(chan struct{})
	cache := newTestCache(t, source, WithCacheBufferSize(16))

	first, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the fetch has published its first buffer, so the second caller
	// certainly arrives while the fetch is in flight.
	head := make([]byte, 16)
	if _, err := io.ReadFull(first, head); err != nil {
		t.Fatalf("reading the first buffer: %v", err)
	}

	second, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	close(source.gate)

	rest := readAllAndClose(t, first)
	if got := append(head, rest...); !slices.Equal(got, content) {
		t.Error("the first reader did not see the whole blob")
	}
	if got := readAllAndClose(t, second); !slices.Equal(got, content) {
		t.Error("the second reader did not see the whole blob")
	}
	if reads := source.readCount(digest); reads != 1 {
		t.Errorf("upstream was read %d times, want 1", reads)
	}
	if stats := cache.Stats(); stats.Deduped != 1 {
		t.Errorf("stats = %+v, want 1 deduplicated read", stats)
	}
}

func TestCachingReaderPublishesAtBufferSize(t *testing.T) {
	content := blobContent(4, 64)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	source.chunk = 8
	source.gateAfter = 8
	source.gate = make(chan struct{})
	cache := newTestCache(t, source, WithCacheBufferSize(8))

	reader, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}

	// The first buffer is available even though the blob is far from complete.
	head := make([]byte, 8)
	if _, err := io.ReadFull(reader, head); err != nil {
		t.Fatalf("reading the first buffer: %v", err)
	}
	if !slices.Equal(head, content[:8]) {
		t.Fatal("the first buffer has the wrong content")
	}

	// Nothing beyond it, until upstream delivers more.
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		io.ReadFull(reader, make([]byte, 1))
	}()
	select {
	case <-blocked:
		t.Fatal("read past the published offset returned before more data arrived")
	case <-time.After(50 * time.Millisecond):
	}

	close(source.gate)
	<-blocked
	readAllAndClose(t, reader)
}

func TestCachingReaderEarlyCloseStillCaches(t *testing.T) {
	content := blobContent(5, 2048)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	cache := newTestCache(t, source, WithCacheBufferSize(256))

	abandoning, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(abandoning, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	completing, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := abandoning.Close(); err != nil {
		t.Fatalf("closing the first reader: %v", err)
	}
	if got := readAllAndClose(t, completing); !slices.Equal(got, content) {
		t.Fatal("the remaining reader did not see the whole blob")
	}

	settle(t, cache)
	assertCached(t, cache, digest)
	if reads := source.readCount(digest); reads != 1 {
		t.Errorf("upstream was read %d times, want 1", reads)
	}
}

func TestCachingReaderAbandonedFetchLeavesNothingBehind(t *testing.T) {
	content := blobContent(6, 64)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	source.chunk = 16
	source.gateAfter = 16
	source.gate = make(chan struct{})
	cache := newTestCache(t, source, WithCacheBufferSize(16))

	reader, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(reader, make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("closing the only reader: %v", err)
	}
	close(source.gate)

	tempDir := filepath.Join(cache.Dir(), tempDirName)
	eventually(t, "the partial blob to be cleaned up", func() bool {
		entries, err := os.ReadDir(tempDir)
		return err == nil && len(entries) == 0
	})
	assertNotCached(t, cache, digest)
}

func TestCachingReaderReaderContextCancellation(t *testing.T) {
	content := blobContent(7, 64)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	source.chunk = 16
	source.gateAfter = 16
	source.gate = make(chan struct{})
	cache := newTestCache(t, source, WithCacheBufferSize(16))

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancelled, err := cache.ReaderForBlob(cancelledCtx, digest)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelled.Close()
	if _, err := io.ReadFull(cancelled, make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	// A second reader shares the same fetch and must survive the first one's
	// cancellation.
	surviving, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}

	blocked := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(cancelled, make([]byte, 1))
		blocked <- err
	}()
	cancel()
	if err := <-blocked; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled read returned %v, want context.Canceled", err)
	}

	close(source.gate)
	if got := readAllAndClose(t, surviving); !slices.Equal(got, content) {
		t.Error("the surviving reader did not see the whole blob")
	}
}

func TestCachingReaderRejectsCorruptContent(t *testing.T) {
	content := blobContent(8, 512)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	source.serveInstead(digest, blobContent(9, 512))
	cache := newTestCache(t, source)

	reader, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("reading a blob whose content does not match its digest succeeded")
	}
	// The reader still holds the partial file, so it is still there: a file with
	// an open handle cannot be removed on Windows, and removing it early would
	// pull it out from under the reader everywhere else.
	tempDir := filepath.Join(cache.Dir(), tempDirName)
	if entries, err := os.ReadDir(tempDir); err != nil || len(entries) != 1 {
		t.Fatalf("temp directory holds %v (err %v), want the partial file the reader has open", entries, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	// Letting go of it is what cleans it up.
	settle(t, cache)
	assertNotCached(t, cache, digest)
}

func TestCachingReaderRejectsTruncatedContent(t *testing.T) {
	content := blobContent(10, 512)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	source.serveInstead(digest, content[:256])
	cache := newTestCache(t, source)

	reader, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("reading a short blob succeeded")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	settle(t, cache)
	assertNotCached(t, cache, digest)
}

func TestCachingReaderRejectsOversizedContent(t *testing.T) {
	content := blobContent(21, 512)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	// A source that keeps sending past the digest's size must not be allowed to
	// write an unbounded amount to disk, and its content must be rejected.
	source.serveInstead(digest, blobContent(22, 4096))
	cache := newTestCache(t, source)

	reader, err := cache.ReaderForBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	if err == nil {
		t.Fatal("reading an oversized blob succeeded")
	}
	if int64(len(got)) > digest.SizeBytes+1 {
		t.Errorf("read %d bytes of an oversized blob, want at most %d", len(got), digest.SizeBytes+1)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	settle(t, cache)
	assertNotCached(t, cache, digest)
}

func TestCachingReaderUpstreamErrorIsReported(t *testing.T) {
	content := blobContent(11, 128)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	source.openErr = errors.New("remote cache unavailable")
	cache := newTestCache(t, source)

	if _, err := cache.ReadBlob(context.Background(), digest); err == nil {
		t.Fatal("ReadBlob succeeded despite an upstream failure")
	}
	// The failed fetch is forgotten, so a later attempt tries again.
	source.openErr = nil
	got, err := cache.ReadBlob(context.Background(), digest)
	if err != nil {
		t.Fatalf("retry after an upstream failure: %v", err)
	}
	if !slices.Equal(got, content) {
		t.Error("the retry returned the wrong bytes")
	}
}

func TestCachingReaderEvictsOnNoSpaceAndRetries(t *testing.T) {
	first := blobContent(12, 1024)
	second := blobContent(13, 1024)
	firstDigest, secondDigest := blobDigest(first), blobDigest(second)
	source := newFakeBlobs(first, second)
	cache := newTestCache(t, source)

	// Cache the first blob so there is something to evict.
	if _, err := cache.ReadBlob(context.Background(), firstDigest); err != nil {
		t.Fatal(err)
	}
	settle(t, cache)
	assertCached(t, cache, firstDigest)

	var failures int
	var mu sync.Mutex
	realWrite := cache.store.write
	cache.store.write = func(f *os.File, p []byte) (int, error) {
		mu.Lock()
		failures++
		fail := failures == 1
		mu.Unlock()
		if fail {
			return 0, &os.PathError{Op: "write", Path: f.Name(), Err: syscall.ENOSPC}
		}
		return realWrite(f, p)
	}

	got, err := cache.ReadBlob(context.Background(), secondDigest)
	if err != nil {
		t.Fatalf("reading a blob after an out-of-space error: %v", err)
	}
	if !slices.Equal(got, second) {
		t.Fatal("the blob read after an out-of-space error is wrong")
	}
	settle(t, cache)
	assertCached(t, cache, secondDigest)
	assertNotCached(t, cache, firstDigest)
	if stats := cache.Stats(); stats.Evicted != 1 || stats.DiskDisabled {
		t.Errorf("stats = %+v, want 1 eviction and caching still enabled", stats)
	}
}

func TestCachingReaderFallsBackWhenDiskIsFull(t *testing.T) {
	content := blobContent(14, 1024)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	cache := newTestCache(t, source)
	cache.store.write = func(f *os.File, p []byte) (int, error) {
		return 0, &os.PathError{Op: "write", Path: f.Name(), Err: syscall.ENOSPC}
	}

	got, err := cache.ReadBlob(context.Background(), digest)
	if err != nil {
		t.Fatalf("reading a blob with a full disk: %v", err)
	}
	if !slices.Equal(got, content) {
		t.Fatal("the blob read with a full disk is wrong")
	}
	stats := cache.Stats()
	if !stats.DiskDisabled {
		t.Error("disk caching should be disabled after an unrecoverable write failure")
	}
	if stats.Fallbacks == 0 {
		t.Error("the read should be counted as a fallback")
	}

	// Later reads skip the disk entirely and still work.
	if _, err := cache.ReadBlob(context.Background(), digest); err != nil {
		t.Fatalf("reading a blob after disk caching was disabled: %v", err)
	}
}

func TestCachingReaderEnforcesMaxSize(t *testing.T) {
	blobs := [][]byte{blobContent(15, 1024), blobContent(16, 1024), blobContent(17, 1024)}
	source := newFakeBlobs(blobs...)
	cache := newTestCache(t, source, WithCacheMaxSize(2048))

	for _, blob := range blobs {
		if _, err := cache.ReadBlob(context.Background(), blobDigest(blob)); err != nil {
			t.Fatal(err)
		}
		settle(t, cache)
	}

	assertNotCached(t, cache, blobDigest(blobs[0]))
	for _, blob := range blobs[1:] {
		assertCached(t, cache, blobDigest(blob))
	}
	if stats := cache.Stats(); stats.Evicted != 1 {
		t.Errorf("stats = %+v, want 1 eviction", stats)
	}
}

func TestCachingReaderFindMissingBlobsAnswersFromCache(t *testing.T) {
	cached := blobContent(18, 256)
	uncached := blobContent(19, 256)
	cachedDigest, uncachedDigest := blobDigest(cached), blobDigest(uncached)
	source := newFakeBlobs(cached, uncached)
	cache := newTestCache(t, source)

	if _, err := cache.ReadBlob(context.Background(), cachedDigest); err != nil {
		t.Fatal(err)
	}
	settle(t, cache)

	// A cached blob is never asked about.
	missing, err := cache.FindMissingBlobs(context.Background(), []Digest{cachedDigest})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("FindMissingBlobs reported %d missing blobs, want 0", len(missing))
	}
	if calls := source.findMissingCalls(); len(calls) != 0 {
		t.Errorf("upstream was asked %d times, want 0", len(calls))
	}

	// Uncached digests still go upstream, and only those.
	if _, err := cache.FindMissingBlobs(context.Background(), []Digest{cachedDigest, uncachedDigest}); err != nil {
		t.Fatal(err)
	}
	calls := source.findMissingCalls()
	if len(calls) != 1 || len(calls[0]) != 1 || calls[0][0].hexHash() != uncachedDigest.hexHash() {
		t.Errorf("upstream was asked about %v, want only the uncached digest", calls)
	}
}

func TestCachingReaderEmptyBlob(t *testing.T) {
	source := newFakeBlobs(nil)
	cache := newTestCache(t, source)
	digest := blobDigest(nil)

	got, err := cache.ReadBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty blob read returned %d bytes", len(got))
	}
	assertNotCached(t, cache, digest)
}

func TestCachingReaderConcurrentReadersShareOneFetch(t *testing.T) {
	content := blobContent(20, 1<<16)
	digest := blobDigest(content)
	source := newFakeBlobs(content)
	source.chunk = 4096
	cache := newTestCache(t, source, WithCacheBufferSize(4096))

	const readers = 8
	var wg sync.WaitGroup
	errs := make([]error, readers)
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := cache.ReadBlob(context.Background(), digest)
			if err != nil {
				errs[i] = err
				return
			}
			if !slices.Equal(got, content) {
				errs[i] = fmt.Errorf("reader %d got %d bytes, want %d", i, len(got), len(content))
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// Every read was either the one fetch, a reader joining it, a hit on what it
	// wrote, or -- if the local copy could not be used -- a fallback.
	stats := cache.Stats()
	if stats.Fetches == 0 || stats.Fetches+stats.Hits+stats.Deduped+stats.Fallbacks < readers {
		t.Errorf("stats = %+v, want every one of the %d reads accounted for", stats, readers)
	}
	if reads, upstream := source.readCount(digest), int(stats.Fetches+stats.Fallbacks); reads > upstream {
		t.Errorf("upstream was read %d times, want at most the %d fetches and fallbacks", reads, upstream)
	}
}
