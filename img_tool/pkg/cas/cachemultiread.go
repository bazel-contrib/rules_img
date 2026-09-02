package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// maxPopulateBytes bounds how much of a blob list a CachingReader fetches --
// and therefore holds in memory -- in one go. It matches the largest batch the
// remote cache will serve, so a window is usually a single BatchReadBlobs
// request. A blob bigger than this is read on its own through the per-blob path,
// which streams it into the cache instead of buffering it.
const maxPopulateBytes = 4 << 20

// ReaderForBlobs reads a whole list of blobs as one stream, serving the blobs
// the cache directory already has from disk and fetching the rest from upstream
// in batches.
//
// The fetched blobs are cached on the way through, so a later read -- in this
// process or the next one -- is a cache hit just as it would be after
// [CachingReader.ReaderForBlob]. Unlike ReaderForBlob, a batched fetch does not
// register itself for in-flight deduplication: a blob another reader is already
// fetching is left to that reader (and read through the per-blob path), but two
// batches that start at the same time may fetch the same blob twice. That costs
// a duplicated download at worst, never correctness.
//
// Local problems never fail a read here either: a blob that cannot be written
// to the cache directory is still delivered, just not cached.
func (c *CachingReader) ReaderForBlobs(ctx context.Context, digests []Digest) (io.ReadCloser, error) {
	// Drop empty blobs: they contribute no bytes, and keeping them would split a
	// window in two for nothing.
	wanted := make([]Digest, 0, len(digests))
	for _, digest := range digests {
		if digest.SizeBytes > 0 {
			wanted = append(wanted, digest)
		}
	}
	return &cachedMultiReader{cache: c, ctx: ctx, digests: wanted}, nil
}

// cachedMultiReader delivers a list of blobs as one stream, pulling the ones the
// cache does not have from upstream a window at a time.
type cachedMultiReader struct {
	cache   *CachingReader
	ctx     context.Context
	digests []Digest

	next    int           // index of the next digest to deliver
	fetched [][]byte      // undelivered blobs of the current window, in order
	cur     io.ReadCloser // per-blob reader for a cache hit or a joined fetch
	err     error         // sticky: the stream delivers nothing after a failure
}

func (r *cachedMultiReader) Read(p []byte) (int, error) {
	for {
		if r.err != nil {
			return 0, r.err
		}
		switch {
		case len(r.fetched) > 0:
			n := copy(p, r.fetched[0])
			r.fetched[0] = r.fetched[0][n:]
			if len(r.fetched[0]) == 0 {
				r.fetched = r.fetched[1:]
			}
			return n, nil
		case r.cur != nil:
			n, err := r.cur.Read(p)
			if err == nil {
				return n, nil
			}
			if !errors.Is(err, io.EOF) {
				r.err = err
				r.closeCurrent()
				return n, r.err
			}
			if closeErr := r.closeCurrent(); closeErr != nil {
				r.err = closeErr
			}
			if n > 0 {
				return n, nil
			}
		case r.next >= len(r.digests):
			return 0, io.EOF
		default:
			if err := r.advance(); err != nil {
				r.err = err
				return 0, r.err
			}
		}
	}
}

// advance readies the next blob (or window of blobs) for delivery.
func (r *cachedMultiReader) advance() error {
	digest := r.digests[r.next]
	// Read a blob on its own when batching would not help: one the cache can
	// already serve -- from disk, or by attaching to a fetch another reader
	// started -- and one too large to hold in memory, which the per-blob path
	// streams through a temp file instead.
	if digest.SizeBytes > maxPopulateBytes || r.cache.cachedOrInflight(digest) {
		reader, err := r.cache.ReaderForBlob(r.ctx, digest)
		if err != nil {
			return err
		}
		r.cur = reader
		r.next++
		return nil
	}
	return r.populate()
}

// populate fetches a window of consecutive blobs the cache does not have, in one
// upstream read, and caches them.
func (r *cachedMultiReader) populate() error {
	end := r.windowEnd()
	window := r.digests[r.next:end]
	stream, err := r.cache.upstream.ReaderForBlobs(r.ctx, window)
	if err != nil {
		return err
	}
	defer stream.Close()

	blobs := make([][]byte, 0, len(window))
	for _, digest := range window {
		data := make([]byte, digest.SizeBytes)
		if _, err := io.ReadFull(stream, data); err != nil {
			return fmt.Errorf("reading blob %s from the remote cache: %w", digest.hexHash(), err)
		}
		if hasher := digestHasher(digest); hasher != nil {
			hasher.Write(data)
			if sum := hasher.Sum(nil); !bytes.Equal(sum, digest.Hash) {
				return fmt.Errorf("blob %s: content from remote cache hashes to %x", digest.hexHash(), sum)
			}
		}
		r.cache.counters.fetches.Add(1)
		r.cache.counters.bytesFetched.Add(digest.SizeBytes)
		r.cache.store.put(digest, data)
		blobs = append(blobs, data)
	}

	r.fetched = blobs
	r.next = end
	return nil
}

// windowEnd returns the end of the run of blobs starting at r.next that is worth
// fetching in one request: consecutive blobs the cache cannot already serve,
// bounded by maxPopulateBytes so a window always fits in memory and by
// maxBatchReadDigests so a run of tiny blobs cannot make it unboundedly long.
// advance has already vetted the blob at r.next, which is what keeps the window
// non-empty.
func (r *cachedMultiReader) windowEnd() int {
	var total int64
	end := r.next
	for end < len(r.digests) && end-r.next < maxBatchReadDigests {
		digest := r.digests[end]
		if end > r.next {
			if digest.SizeBytes > maxPopulateBytes || r.cache.cachedOrInflight(digest) {
				break
			}
			if total+digest.SizeBytes > maxPopulateBytes {
				break
			}
		}
		total += digest.SizeBytes
		end++
	}
	return end
}

func (r *cachedMultiReader) closeCurrent() error {
	if r.cur == nil {
		return nil
	}
	err := r.cur.Close()
	r.cur = nil
	return err
}

func (r *cachedMultiReader) Close() error {
	r.next = len(r.digests)
	r.fetched = nil
	return r.closeCurrent()
}

// cachedOrInflight reports whether ReaderForBlob can serve the blob without
// starting a new upstream read, either from the cache directory or by attaching
// to a fetch that is already running.
func (c *CachingReader) cachedOrInflight(digest Digest) bool {
	c.mu.Lock()
	_, inflight := c.inflight[digest.cacheKey()]
	c.mu.Unlock()
	return inflight || c.store.has(digest)
}
