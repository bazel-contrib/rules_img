package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeChunkSource is a blob list of fixed-size chunks, with a hook that lets a
// test hold fetches open and watch how many run at once.
type fakeChunkSource struct {
	blobs     [][]byte // one entry per blob
	perChunk  int      // blobs per chunk
	streamAt  map[int]bool
	fetchErr  map[int]error // keyed by chunk start index
	gate      chan struct{} // if non-nil, every fetch waits on it
	blockUpTo int           // only the first n fetches wait on the gate

	mu       sync.Mutex
	started  []int // chunk start index, in the order fetches began
	inFlight int
	peak     int
	opened   []int // chunk start index of every streamed chunk opened
}

func (s *fakeChunkSource) count() int { return len(s.blobs) }

func (s *fakeChunkSource) plan(i int) chunk {
	if s.streamAt[i] {
		return chunk{from: i, to: i + 1, bytes: int64(len(s.blobs[i])), streamed: true}
	}
	end := min(i+s.perChunk, len(s.blobs))
	var total int64
	for _, blob := range s.blobs[i:end] {
		total += int64(len(blob))
	}
	return chunk{from: i, to: end, bytes: total}
}

func (s *fakeChunkSource) fetch(ctx context.Context, c chunk) ([][]byte, error) {
	s.mu.Lock()
	s.started = append(s.started, c.from)
	s.inFlight++
	s.peak = max(s.peak, s.inFlight)
	gated := s.gate != nil && len(s.started) <= s.blockUpTo
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()

	if gated {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := s.fetchErr[c.from]; err != nil {
		return nil, err
	}
	return slices.Clone(s.blobs[c.from:c.to]), nil
}

func (s *fakeChunkSource) open(_ context.Context, c chunk) (io.ReadCloser, error) {
	s.mu.Lock()
	s.opened = append(s.opened, c.from)
	s.mu.Unlock()
	return io.NopCloser(bytes.NewReader(s.blobs[c.from])), nil
}

func (s *fakeChunkSource) startOrder() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.started)
}

func (s *fakeChunkSource) peakInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

func (s *fakeChunkSource) openedChunks() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.opened)
}

func chunkyBlobs(n, size int) [][]byte {
	blobs := make([][]byte, n)
	for i := range blobs {
		blobs[i] = bytes.Repeat([]byte{byte('a' + i%26)}, size)
	}
	return blobs
}

func testBudget(limit int64) *prefetchBudget {
	return &prefetchBudget{limit: func() int64 { return limit }}
}

// waitFor polls until cond holds, so a test can watch work that a background
// goroutine does without pinning it to a sleep.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The point of prefetching: the next batch is already being fetched while the
// consumer is still working through the last one.
func TestPrefetchRunsBatchesConcurrently(t *testing.T) {
	source := &fakeChunkSource{
		blobs:     chunkyBlobs(9, 100),
		perChunk:  3,
		gate:      make(chan struct{}),
		blockUpTo: 3,
	}
	r := newPrefetchingReader(context.Background(), source, testBudget(1<<20))
	defer r.Close()

	// Nothing has been read yet, and several fetches are already under way.
	waitFor(t, "concurrent fetches", func() bool { return source.peakInFlight() >= 2 })
	close(source.gate)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Join(source.blobs, nil); !bytes.Equal(got, want) {
		t.Fatal("prefetching changed the stream")
	}
	if peak := source.peakInFlight(); peak < 2 {
		t.Fatalf("peak concurrent fetches = %d, want at least 2", peak)
	}
	if peak := source.peakInFlight(); peak > prefetchDepth+1 {
		t.Fatalf("peak concurrent fetches = %d, over the depth of %d", peak, prefetchDepth)
	}
}

// Chunks are delivered in list order however their fetches finish.
func TestPrefetchDeliversInOrderDespiteOutOfOrderFetches(t *testing.T) {
	source := &fakeChunkSource{blobs: chunkyBlobs(12, 64), perChunk: 2}
	// Finish the later chunks first by holding the first one back.
	first := make(chan struct{})
	source.gate = first
	source.blockUpTo = 1

	r := newPrefetchingReader(context.Background(), source, testBudget(1<<20))
	defer r.Close()

	waitFor(t, "later chunks to start", func() bool { return len(source.startOrder()) >= 2 })
	close(first)

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Join(source.blobs, nil); !bytes.Equal(got, want) {
		t.Fatal("stream is out of order")
	}
}

// A streamed chunk is opened only when the consumer reaches it: holding a gRPC
// stream open while nothing reads it would trip the idle timeout.
func TestPrefetchDoesNotOpenStreamedChunksAhead(t *testing.T) {
	source := &fakeChunkSource{
		blobs:    chunkyBlobs(6, 32),
		perChunk: 1,
		streamAt: map[int]bool{3: true, 4: true, 5: true},
	}
	r := newPrefetchingReader(context.Background(), source, testBudget(1<<20))
	defer r.Close()

	// Read the first blob only.
	if _, err := io.ReadFull(r, make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	if opened := source.openedChunks(); len(opened) != 0 {
		t.Fatalf("streamed chunks %v were opened before the consumer reached them", opened)
	}

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Join(source.blobs[1:], nil); !bytes.Equal(got, want) {
		t.Fatal("streamed chunks did not deliver their bytes in order")
	}
	if opened := source.openedChunks(); !slices.Equal(opened, []int{3, 4, 5}) {
		t.Fatalf("opened %v, want [3 4 5] in order", opened)
	}
}

// The budget bounds how far ahead a reader runs. With room for one chunk, the
// second waits until the first is delivered.
func TestPrefetchBudgetBoundsReadAhead(t *testing.T) {
	const chunkBytes = 300 // 3 blobs of 100
	source := &fakeChunkSource{blobs: chunkyBlobs(9, 100), perChunk: 3}
	budget := testBudget(chunkBytes)

	r := newPrefetchingReader(context.Background(), source, budget)
	defer r.Close()

	waitFor(t, "the first chunk to start", func() bool { return len(source.startOrder()) >= 1 })
	// A reader holding a chunk may not take another until it gives one back.
	time.Sleep(20 * time.Millisecond)
	if started := source.startOrder(); len(started) != 1 {
		t.Fatalf("%d chunks started with room for one, want 1", len(started))
	}

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Join(source.blobs, nil); !bytes.Equal(got, want) {
		t.Fatal("a tight budget changed the stream")
	}
	if used := budget.inUse(); used != 0 {
		t.Fatalf("%d bytes still reserved after the stream was drained", used)
	}
}

// A budget of zero turns read-ahead off without stalling: the reader still
// makes progress a chunk at a time.
func TestPrefetchBudgetZeroStillReads(t *testing.T) {
	source := &fakeChunkSource{blobs: chunkyBlobs(6, 50), perChunk: 2}
	budget := testBudget(0)

	r := newPrefetchingReader(context.Background(), source, budget)
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.Join(source.blobs, nil); !bytes.Equal(got, want) {
		t.Fatal("a zero budget changed the stream")
	}
	if peak := source.peakInFlight(); peak > 1 {
		t.Fatalf("peak concurrent fetches = %d with no budget, want 1", peak)
	}
	if used := budget.inUse(); used != 0 {
		t.Fatalf("%d bytes still reserved after the stream was drained", used)
	}
}

// Closing partway through gives back everything the reader had reserved.
func TestPrefetchCloseReleasesTheBudget(t *testing.T) {
	source := &fakeChunkSource{blobs: chunkyBlobs(30, 100), perChunk: 3}
	budget := testBudget(1 << 20)

	r := newPrefetchingReader(context.Background(), source, budget)
	if _, err := io.ReadFull(r, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "read-ahead", func() bool { return budget.inUse() > 0 })

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if used := budget.inUse(); used != 0 {
		t.Fatalf("%d bytes still reserved after Close", used)
	}
	// A read after Close ends the stream rather than reporting the cancellation.
	if n, err := r.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("read after Close = (%d, %v), want (0, EOF)", n, err)
	}
	// Close is idempotent.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}

// A fetch failure reaches the consumer instead of truncating the stream, and
// stays reported.
func TestPrefetchReportsAFetchFailure(t *testing.T) {
	want := errors.New("upstream is unhappy")
	source := &fakeChunkSource{
		blobs:    chunkyBlobs(9, 100),
		perChunk: 3,
		fetchErr: map[int]error{3: want},
	}
	r := newPrefetchingReader(context.Background(), source, testBudget(1<<20))
	defer r.Close()

	_, err := io.ReadAll(r)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, want) {
		t.Fatalf("error after failure = %v, want %v", err, want)
	}
}

// A cancelled context ends the stream with the cancellation, not a silent EOF
// that would look like a complete but truncated layer.
func TestPrefetchReportsCancellationNotEOF(t *testing.T) {
	source := &fakeChunkSource{
		blobs:     chunkyBlobs(9, 100),
		perChunk:  3,
		gate:      make(chan struct{}),
		blockUpTo: 100,
	}
	defer close(source.gate)

	ctx, cancel := context.WithCancel(context.Background())
	r := newPrefetchingReader(ctx, source, testBudget(1<<20))
	defer r.Close()

	waitFor(t, "the first fetch to start", func() bool { return len(source.startOrder()) >= 1 })
	cancel()

	if _, err := io.ReadAll(r); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want a cancellation", err)
	}
}

// A reader that holds nothing is admitted whatever the budget says, so a full
// budget cannot stop one from making progress.
func TestPrefetchBudgetAlwaysAdmitsAnEmptyHolder(t *testing.T) {
	budget := testBudget(100)
	stop := make(chan struct{})

	var hog atomic.Int64
	if !budget.reserve(context.Background(), stop, 100, &hog) {
		t.Fatal("the first reservation was refused")
	}
	if used := budget.inUse(); used != 100 {
		t.Fatalf("budget in use = %d, want 100", used)
	}

	// A second reader holding nothing gets in even though the budget is spent.
	var newcomer atomic.Int64
	done := make(chan bool, 1)
	go func() { done <- budget.reserve(context.Background(), stop, 50, &newcomer) }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("a reader holding nothing was refused")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a reader holding nothing was made to wait")
	}

	// The one already holding bytes has to wait for room.
	blocked := make(chan bool, 1)
	go func() { blocked <- budget.reserve(context.Background(), stop, 50, &hog) }()
	select {
	case <-blocked:
		t.Fatal("a reader holding bytes was admitted over the budget")
	case <-time.After(50 * time.Millisecond):
	}

	budget.release(100, &hog)
	budget.release(50, &newcomer)
	select {
	case ok := <-blocked:
		if !ok {
			t.Fatal("the waiting reservation was refused after room freed up")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("releasing bytes did not wake the waiting reservation")
	}
	budget.release(50, &hog)
	if used := budget.inUse(); used != 0 {
		t.Fatalf("budget in use = %d, want 0", used)
	}
}

// A reservation gives up when the reader stops.
func TestPrefetchBudgetReserveHonorsStop(t *testing.T) {
	budget := testBudget(10)
	stop := make(chan struct{})

	var held atomic.Int64
	if !budget.reserve(context.Background(), stop, 10, &held) {
		t.Fatal("the first reservation was refused")
	}
	done := make(chan bool, 1)
	go func() { done <- budget.reserve(context.Background(), stop, 10, &held) }()
	close(stop)
	select {
	case ok := <-done:
		if ok {
			t.Fatal("a reservation succeeded after the reader stopped")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stopping did not end the wait")
	}
}

// Concurrent batches of one stream go out on different pool connections: that
// is what the pool is for.
func TestReaderForBlobsSpreadsBatchesAcrossThePool(t *testing.T) {
	blobs := smallBlobs(6, 100)
	clients := make([]*batchCASClient, 3)
	members := make([]*CAS, len(clients))
	for i := range clients {
		clients[i] = newBatchCASClient(blobs...)
		members[i] = testCAS(nil, clients[i])
		// One blob per batch, so six blobs make six batches.
		members[i].capabilities.MaxBatchTotalSizeBytes = 100 + batchReadEntryOverhead
	}
	NewPool(members)

	got := readBlobs(t, members[0], digestsOf(clients[0], blobs))
	if want := bytes.Join(blobs, nil); !bytes.Equal(got, want) {
		t.Fatal("spreading across the pool changed the stream")
	}

	var used int
	total := 0
	for _, client := range clients {
		if n := client.requestCount(); n > 0 {
			used++
			total += n
		}
	}
	if total != len(blobs) {
		t.Fatalf("made %d requests in total, want %d", total, len(blobs))
	}
	if used < 2 {
		t.Fatalf("all %d batches went to one connection; the pool was not used", total)
	}
}

// Prefetching must not change what a compact-stream-sized read returns.
func TestReaderForBlobsPrefetchPreservesContent(t *testing.T) {
	blobs := smallBlobs(500, 200)
	client := newBatchCASClient(blobs...)
	c := testCAS(nil, client)
	c.capabilities.MaxBatchTotalSizeBytes = 4 * 1024

	got := readBlobs(t, c, digestsOf(client, blobs))
	if want := bytes.Join(blobs, nil); !bytes.Equal(got, want) {
		t.Fatalf("stream is %d bytes, want %d", len(got), len(want))
	}
	if client.requestCount() < 2 {
		t.Fatalf("made %d requests; the read was not batched at all", client.requestCount())
	}
}

// The caching reader prefetches its windows too, which is the layer the deploy
// path actually goes through.
func TestCachingReaderPrefetchesWindows(t *testing.T) {
	blobs := make([][]byte, 12)
	for i := range blobs {
		blobs[i] = []byte(fmt.Sprintf("cached-blob-%02d", i))
	}
	source := newBatchRecordingBlobs(blobs...)
	source.hold = make(chan struct{})
	cache := newTestCache(t, source)

	digests := make([]Digest, len(blobs))
	for i, blob := range blobs {
		digests[i] = blobDigest(blob)
	}
	rc, err := cache.ReaderForBlobs(context.Background(), digests)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	// Nothing read yet, and the fetch is already under way.
	waitFor(t, "a window fetch to start", func() bool { return source.startedLists() >= 1 })
	close(source.hold)

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	for _, blob := range blobs {
		want.Write(blob)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatal("prefetching changed what the caching reader delivered")
	}
}
