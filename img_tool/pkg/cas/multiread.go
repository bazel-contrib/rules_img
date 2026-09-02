package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sync/atomic"

	"google.golang.org/grpc/status"

	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

// maxBatchReadDigests bounds how many digests one BatchReadBlobs request names,
// independently of the server's byte limit. A compact stream can reference tens
// of thousands of tiny blobs, and a request naming all of them would be a very
// large message for a server that only advertised a limit on the bytes it
// returns.
const maxBatchReadDigests = 1024

// batchReadEntryOverhead is what one blob costs a BatchReadBlobs exchange beyond
// its own bytes: in the request a Digest (a 64-character hex hash, a size and
// their field tags and length delimiters, ~75 bytes), and in the response that
// same Digest again inside a Response entry, plus the entry's and the data
// field's own tags and delimiters. 128 bytes rounds that up.
//
// batchPayloadBudget already holds back batchFramingHeadroom for framing, but
// that reservation is a fixed 1 KiB sized for a request naming a single blob.
// Framing on a batch scales with the number of blobs -- a thousand of them spend
// eighty times the reservation -- so a batch charges each blob its own share as
// well, which keeps the message inside the transport limit however small the
// blobs are.
const batchReadEntryOverhead = 128

// ReaderForBlobs returns a reader over the concatenation of the given blobs, in
// the order requested. Reading it start to finish yields exactly what reading
// each blob with [CAS.ReaderForBlob] in turn would, and empty blobs contribute
// nothing.
//
// Knowing the whole list up front is what makes it fast: consecutive blobs small
// enough to batch are read with a single BatchReadBlobs request holding as many
// of them as the server's MaxBatchTotalSizeBytes allows, so a caller reading
// thousands of small blobs pays a round trip per few megabytes rather than one
// per blob. Blobs too large to batch are read with ByteStream, one at a time, as
// before.
//
// Batches are fetched ahead of the consumer, several at a time and spread across
// the connection pool, so the round trip for the next one is already under way
// while the last is being read. Memory is bounded by the shared prefetch budget
// (see [EnvPrefetchBytes]) no matter how long the list is. The reader is not safe
// for concurrent use.
func (c *CAS) ReaderForBlobs(ctx context.Context, digests []Digest) (io.ReadCloser, error) {
	if len(digests) == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	// One BatchReadBlobs request carries a single digest function, and mixing
	// algorithms in one stream has no use here.
	for _, d := range digests {
		if !c.capabilities.supportedDigestFunction(d.algorithm) {
			return nil, fmt.Errorf("unsupported digest algorithm: %s", d.algorithm)
		}
		if d.algorithm != digests[0].algorithm {
			return nil, fmt.Errorf("all digests must use the same algorithm: %s != %s", d.algorithm, digests[0].algorithm)
		}
	}
	// Drop empty blobs: they contribute no bytes, and keeping them would split a
	// batch in two for nothing.
	wanted := make([]Digest, 0, len(digests))
	for _, d := range digests {
		if d.SizeBytes > 0 {
			wanted = append(wanted, d)
		}
	}
	return newPrefetchingReader(ctx, &batchSource{owner: c, digests: wanted}, sharedPrefetchBudget), nil
}

// batchSource is a list of blobs read from the remote cache: runs small enough
// to batch become one BatchReadBlobs request each, and a blob too large for one
// is streamed on its own.
type batchSource struct {
	owner   *CAS
	digests []Digest

	// batches counts the batches planned so far, which spreads their requests
	// across the connection pool: prefetching puts several in flight at once, and
	// a pool exists because one connection caps bulk throughput.
	batches atomic.Uint64
}

var _ chunkSource = (*batchSource)(nil)

func (s *batchSource) count() int { return len(s.digests) }

// plan groups the digests from i onwards into the largest run that fits in one
// BatchReadBlobs request. Each blob is charged its framing as well as its bytes
// (see batchReadEntryOverhead), and a blob that does not fit even alone is
// streamed instead -- which is also what keeps a run from coming out empty.
func (s *batchSource) plan(i int) chunk {
	limit := s.owner.capabilities.MaxBatchTotalSizeBytes
	if s.digests[i].SizeBytes+batchReadEntryOverhead > limit {
		return chunk{from: i, to: i + 1, bytes: s.digests[i].SizeBytes, streamed: true}
	}
	var total int64
	end := i
	for end < len(s.digests) && end-i < maxBatchReadDigests {
		cost := s.digests[end].SizeBytes + batchReadEntryOverhead
		if total+cost > limit {
			break
		}
		total += cost
		end++
	}
	return chunk{from: i, to: end, bytes: total}
}

func (s *batchSource) fetch(ctx context.Context, c chunk) ([][]byte, error) {
	// peer(0) is this client and peer(n) walks the pool, so concurrent batches
	// of one stream land on different connections. The mask only keeps the
	// counter from indexing negatively once it wraps.
	peer := s.owner.peer(int((s.batches.Add(1) - 1) & math.MaxInt32))
	return peer.batchRead(ctx, s.digests[c.from:c.to])
}

func (s *batchSource) open(ctx context.Context, c chunk) (io.ReadCloser, error) {
	return s.owner.streamReadOne(ctx, s.digests[c.from])
}

// batchRead reads a run of blobs with a single BatchReadBlobs request and
// returns their contents in request order.
func (c *CAS) batchRead(ctx context.Context, digests []Digest) ([][]byte, error) {
	// The same blob may be referenced more than once in a stream; ask for it once
	// and hand out the bytes twice.
	unique := make([]*remoteexecution_proto.Digest, 0, len(digests))
	seen := make(map[string]struct{}, len(digests))
	var total int64
	for _, d := range digests {
		total += d.SizeBytes
		if _, ok := seen[d.hexHash()]; ok {
			continue
		}
		seen[d.hexHash()] = struct{}{}
		unique = append(unique, d.protoDigest())
	}
	request := &remoteexecution_proto.BatchReadBlobsRequest{
		InstanceName:   c.instanceName,
		Digests:        unique,
		DigestFunction: digests[0].protoDigestFunction(),
	}

	r := c.retry.start(batchReadDescription(digests, unique, total))
	for {
		blobs, err := c.peer(r.attempt).batchReadOnce(ctx, request, digests)
		if err == nil {
			return blobs, nil
		}
		if giveUp := r.next(ctx, err); giveUp != nil {
			return nil, fmt.Errorf("failed to read blob(s): %w", casErr(giveUp))
		}
	}
}

// batchReadDescription names a batch read in retry warnings and in the final
// error.
func batchReadDescription(digests []Digest, unique []*remoteexecution_proto.Digest, total int64) string {
	if len(unique) == 1 {
		return fmt.Sprintf("reading blob %s (%d bytes) from the remote cache", digests[0].hexHash(), digests[0].SizeBytes)
	}
	return fmt.Sprintf("reading %d blobs (%d bytes) from the remote cache", len(unique), total)
}

// batchReadOnce is one BatchReadBlobs call. A transient per-blob status is
// returned as a status error, so the retry loop treats it like a transient
// call-level failure and repeats the whole request: the spec has BatchReadBlobs
// report each blob's outcome individually, and re-reading a content-addressed
// blob is always safe.
//
// Responses are matched to requests by digest rather than by position: the spec
// does not promise an order, and a request that asked for a duplicated blob only
// once still has to fill both of its places in the result.
func (c *CAS) batchReadOnce(ctx context.Context, request *remoteexecution_proto.BatchReadBlobsRequest, digests []Digest) ([][]byte, error) {
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	resp, err := c.casClient.BatchReadBlobs(callCtx, request)
	if err != nil {
		return nil, err
	}

	byHash := make(map[string][]byte, len(resp.Responses))
	for _, blob := range resp.Responses {
		if st := blob.Status; st != nil && st.Code != 0 {
			return nil, status.ErrorProto(st)
		}
		if blob.Digest == nil {
			return nil, errors.New("BatchReadBlobs response entry has no digest")
		}
		if blob.Compressor != remoteexecution_proto.Compressor_IDENTITY {
			return nil, fmt.Errorf("blob %s: remote cache returned %s-compressed data, which was not requested", blob.Digest.Hash, blob.Compressor)
		}
		byHash[blob.Digest.Hash] = blob.Data
	}

	blobs := make([][]byte, len(digests))
	for i, d := range digests {
		data, ok := byHash[d.hexHash()]
		if !ok {
			return nil, fmt.Errorf("blob %s: missing from the BatchReadBlobs response", d.hexHash())
		}
		if int64(len(data)) != d.SizeBytes {
			return nil, fmt.Errorf("blob %s: remote cache returned %d bytes, expected %d", d.hexHash(), len(data), d.SizeBytes)
		}
		blobs[i] = data
	}
	return blobs, nil
}
