package cas

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// maxPopulateBytes bounds how much of a blob list a CachingReader fetches in one
// go. It matches the largest batch the remote cache will serve, so a window is
// usually a single BatchReadBlobs request. A blob bigger than this is read on its
// own through the per-blob path, which streams it into the cache instead of
// buffering it.
//
// How many windows may be in flight, and how much they may hold between them, is
// the shared prefetch budget's business (see [EnvPrefetchBytes]).
const maxPopulateBytes = 4 << 20

// ReaderForBlobs reads a whole list of blobs as one stream, serving the blobs
// the cache directory already has from disk and fetching the rest from upstream
// in batches, several of them in flight at a time.
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
	return newPrefetchingReader(ctx, &cachedSource{cache: c, digests: wanted}, sharedPrefetchBudget), nil
}

// cachedSource is a list of blobs served from the cache directory where it can
// be, and fetched from upstream a window at a time where it cannot.
type cachedSource struct {
	cache   *CachingReader
	digests []Digest
}

var _ chunkSource = (*cachedSource)(nil)

func (s *cachedSource) count() int { return len(s.digests) }

// plan groups the digests from i onwards into a window worth fetching in one go:
// consecutive blobs the cache cannot already serve, bounded by maxPopulateBytes
// and by maxBatchReadDigests so a run of tiny blobs cannot make it unboundedly
// long.
//
// A blob the cache can serve -- from disk, or by attaching to a fetch another
// reader started -- is a chunk of its own, streamed rather than fetched: it
// costs nothing to read on its own, and joining that fetch beats starting a
// second one. So is a blob too large to hold in memory, which the per-blob path
// streams through a temp file.
func (s *cachedSource) plan(i int) chunk {
	if !s.batchable(s.digests[i]) {
		return chunk{from: i, to: i + 1, bytes: s.digests[i].SizeBytes, streamed: true}
	}
	var total int64
	end := i
	for end < len(s.digests) && end-i < maxBatchReadDigests {
		digest := s.digests[end]
		if end > i && (!s.batchable(digest) || total+digest.SizeBytes > maxPopulateBytes) {
			break
		}
		total += digest.SizeBytes
		end++
	}
	return chunk{from: i, to: end, bytes: total}
}

// batchable reports whether a blob is worth putting in a window at all.
func (s *cachedSource) batchable(digest Digest) bool {
	return digest.SizeBytes <= maxPopulateBytes && !s.cache.cachedOrInflight(digest)
}

// fetch reads a window from upstream in one call and caches every blob in it.
func (s *cachedSource) fetch(ctx context.Context, c chunk) ([][]byte, error) {
	window := s.digests[c.from:c.to]
	stream, err := s.cache.upstream.ReaderForBlobs(ctx, window)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	blobs := make([][]byte, 0, len(window))
	for _, digest := range window {
		data := make([]byte, digest.SizeBytes)
		if _, err := io.ReadFull(stream, data); err != nil {
			return nil, fmt.Errorf("reading blob %s from the remote cache: %w", digest.hexHash(), err)
		}
		if hasher := digestHasher(digest); hasher != nil {
			hasher.Write(data)
			if sum := hasher.Sum(nil); !bytes.Equal(sum, digest.Hash) {
				return nil, fmt.Errorf("blob %s: content from remote cache hashes to %x", digest.hexHash(), sum)
			}
		}
		s.cache.counters.fetches.Add(1)
		s.cache.counters.bytesFetched.Add(digest.SizeBytes)
		s.cache.store.put(digest, data)
		blobs = append(blobs, data)
	}
	return blobs, nil
}

func (s *cachedSource) open(ctx context.Context, c chunk) (io.ReadCloser, error) {
	return s.cache.ReaderForBlob(ctx, s.digests[c.from])
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
