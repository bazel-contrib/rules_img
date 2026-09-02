package cas

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
)

// batchRecordingBlobs is a BlobSource whose ReaderForBlobs records the lists it
// was asked for, so a test can tell a batched fetch from a per-blob one.
type batchRecordingBlobs struct {
	*fakeBlobs

	mu    sync.Mutex
	lists [][]Digest
}

func newBatchRecordingBlobs(blobs ...[]byte) *batchRecordingBlobs {
	return &batchRecordingBlobs{fakeBlobs: newFakeBlobs(blobs...)}
}

func (b *batchRecordingBlobs) ReaderForBlobs(ctx context.Context, digests []Digest) (io.ReadCloser, error) {
	b.mu.Lock()
	b.lists = append(b.lists, slices.Clone(digests))
	b.mu.Unlock()

	var buf bytes.Buffer
	for _, digest := range digests {
		data, err := b.fakeBlobs.ReadBlob(ctx, digest)
		if err != nil {
			return nil, err
		}
		buf.Write(data)
	}
	return io.NopCloser(&buf), nil
}

func (b *batchRecordingBlobs) batchLists() [][]Digest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.lists)
}

func readCachedBlobs(t *testing.T, c *CachingReader, digests []Digest) []byte {
	t.Helper()
	rc, err := c.ReaderForBlobs(context.Background(), digests)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCachingReaderBatchesUncachedBlobs(t *testing.T) {
	blobs := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	source := newBatchRecordingBlobs(blobs...)
	cache := newTestCache(t, source)

	digests := make([]Digest, len(blobs))
	for i, blob := range blobs {
		digests[i] = blobDigest(blob)
	}

	got := readCachedBlobs(t, cache, digests)
	if want := "onetwothree"; string(got) != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
	lists := source.batchLists()
	if len(lists) != 1 || len(lists[0]) != 3 {
		t.Fatalf("upstream saw %v, want one list of 3 blobs", lists)
	}
}

// A batched read populates the cache, so reading the same blobs again touches
// upstream not at all.
func TestCachingReaderCachesBatchedBlobs(t *testing.T) {
	blobs := [][]byte{[]byte("alpha"), []byte("beta")}
	source := newBatchRecordingBlobs(blobs...)
	cache := newTestCache(t, source)

	digests := []Digest{blobDigest(blobs[0]), blobDigest(blobs[1])}
	first := readCachedBlobs(t, cache, digests)
	second := readCachedBlobs(t, cache, digests)

	if !bytes.Equal(first, second) {
		t.Fatalf("second read = %q, want %q", second, first)
	}
	if lists := source.batchLists(); len(lists) != 1 {
		t.Fatalf("upstream saw %d batched lists, want 1", len(lists))
	}
	for _, digest := range digests {
		if got := source.readCount(digest); got != 1 {
			t.Errorf("blob %s was read %d times upstream, want 1", digest.hexHash(), got)
		}
	}
}

// Blobs already on disk are served from there, and only the gaps are fetched --
// as one batch each, not one request per blob.
func TestCachingReaderBatchesOnlyTheGaps(t *testing.T) {
	blobs := [][]byte{[]byte("aa"), []byte("bb"), []byte("cc"), []byte("dd"), []byte("ee")}
	source := newBatchRecordingBlobs(blobs...)
	cache := newTestCache(t, source)

	digests := make([]Digest, len(blobs))
	for i, blob := range blobs {
		digests[i] = blobDigest(blob)
	}
	// Warm the cache with the middle blob so the run is split in two.
	if _, err := cache.ReadBlob(context.Background(), digests[2]); err != nil {
		t.Fatal(err)
	}
	source.lists = nil

	got := readCachedBlobs(t, cache, digests)
	if want := "aabbccddee"; string(got) != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
	lists := source.batchLists()
	if len(lists) != 2 {
		t.Fatalf("upstream saw %d batched lists, want 2 (one on each side of the cached blob)", len(lists))
	}
	if len(lists[0]) != 2 || len(lists[1]) != 2 {
		t.Fatalf("batched lists have sizes %d and %d, want 2 and 2", len(lists[0]), len(lists[1]))
	}
	if got := source.readCount(digests[2]); got != 1 {
		t.Fatalf("the cached blob was read %d times upstream, want 1 (the warm-up)", got)
	}
}

// Content that does not hash to its digest never reaches the caller.
func TestCachingReaderRejectsCorruptBatchedContent(t *testing.T) {
	blob := []byte("the real content")
	source := newBatchRecordingBlobs(blob)
	digest := blobDigest(blob)
	source.serveInstead(digest, []byte("different content"))
	cache := newTestCache(t, source)

	rc, err := cache.ReaderForBlobs(context.Background(), []Digest{digest})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("error = %v, want a content mismatch", err)
	}
}

func TestCachingReaderMultiReadSkipsEmptyBlobs(t *testing.T) {
	blob := []byte("content")
	source := newBatchRecordingBlobs(blob)
	cache := newTestCache(t, source)

	digests := []Digest{blobDigest(nil), blobDigest(blob)}
	if got := readCachedBlobs(t, cache, digests); string(got) != "content" {
		t.Fatalf("stream = %q, want %q", got, "content")
	}
}

// A blob too large to hold in memory takes the per-blob path, which streams it
// through a temp file, rather than being buffered by a batched fetch.
func TestCachingReaderStreamsLargeBlobsInsteadOfBatching(t *testing.T) {
	small := []byte("small")
	large := bytes.Repeat([]byte("L"), maxPopulateBytes+1)
	source := newBatchRecordingBlobs(small, large)
	cache := newTestCache(t, source)

	digests := []Digest{blobDigest(small), blobDigest(large), blobDigest(small)}
	got := readCachedBlobs(t, cache, digests)
	if want := len(small)*2 + len(large); len(got) != want {
		t.Fatalf("stream is %d bytes, want %d", len(got), want)
	}
	for _, list := range source.batchLists() {
		for _, digest := range list {
			if digest.SizeBytes > maxPopulateBytes {
				t.Fatalf("the %d byte blob was fetched as part of a batch", digest.SizeBytes)
			}
		}
	}
}

// An upstream failure is reported and stays reported.
func TestCachingReaderMultiReadPropagatesUpstreamFailure(t *testing.T) {
	blob := []byte("content")
	source := newBatchRecordingBlobs(blob)
	source.openErr = errors.New("upstream is down")
	cache := newTestCache(t, source)

	rc, err := cache.ReaderForBlobs(context.Background(), []Digest{blobDigest(blob)})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "upstream is down") {
		t.Fatalf("error = %v, want the upstream failure", err)
	}
	if _, err := rc.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "upstream is down") {
		t.Fatalf("error after failure = %v, want the upstream failure again", err)
	}
}
