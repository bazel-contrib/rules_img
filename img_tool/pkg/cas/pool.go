package cas

import (
	"context"
	"io"
	"sync/atomic"

	"google.golang.org/grpc/codes"
)

// BlobSource is the CAS read surface: the subset of *CAS a Pool distributes
// reads across, and the upstream a CachingReader wraps. Exposing it as an
// interface also lets both be unit-tested with fakes.
type BlobSource interface {
	FindMissingBlobs(ctx context.Context, digests []Digest) ([]Digest, error)
	ReadBlob(ctx context.Context, digest Digest) ([]byte, error)
	ReaderForBlob(ctx context.Context, digest Digest) (io.ReadCloser, error)
	// ReaderForBlobs reads a whole list of blobs as one stream: the
	// concatenation of the blobs, in the order requested. It is what
	// ReaderForBlob is to a single blob, and exists so that an implementation
	// can serve many small blobs without a round trip each.
	ReaderForBlobs(ctx context.Context, digests []Digest) (io.ReadCloser, error)
}

var (
	_ BlobSource = (*CAS)(nil)
	_ BlobSource = (*Pool)(nil)
)

// Pool spreads CAS reads across several independent gRPC connections.
//
// A single gRPC ClientConn multiplexes every RPC onto one HTTP/2 (TCP)
// connection, which shares a single congestion window and receive buffer. On
// high-latency links that caps bulk-download throughput no matter how many
// reads run concurrently. A Pool round-robins reads across several members —
// each backed by its own ClientConn/TCP connection — so aggregate throughput
// can scale with the number of connections. This mirrors the connection
// pooling behind Bazel's --remote_max_connections.
//
// A Pool exposes the same read surface as *CAS (FindMissingBlobs, ReadBlob,
// ReaderForBlob) and is safe for concurrent use.
type Pool struct {
	members []BlobSource
	next    atomic.Uint64
}

// NewPool returns a Pool that round-robins reads across the given CAS members.
// Each member should be backed by its own gRPC connection for pooling to have
// any effect. NewPool panics if members is empty; a single-member pool is
// valid and behaves like the member itself.
//
// Pooling also gives retries somewhere to go: the members become each other's
// peers, so an attempt that failed on one connection is retried on the next
// (cf. Bazel taking a channel from its pool per attempt).
func NewPool(members []*CAS) *Pool {
	sources := make([]BlobSource, len(members))
	for i, m := range members {
		m.peers = members
		m.peerIndex = i
		sources[i] = m
	}
	return newPool(sources)
}

func newPool(members []BlobSource) *Pool {
	if len(members) == 0 {
		panic("cas.NewPool: at least one member is required")
	}
	return &Pool{members: members}
}

// pick returns the next member in round-robin order.
func (p *Pool) pick() BlobSource {
	if len(p.members) == 1 {
		return p.members[0]
	}
	i := p.next.Add(1) - 1
	return p.members[i%uint64(len(p.members))]
}

func (p *Pool) FindMissingBlobs(ctx context.Context, digests []Digest) ([]Digest, error) {
	return p.pick().FindMissingBlobs(ctx, digests)
}

func (p *Pool) ReadBlob(ctx context.Context, digest Digest) ([]byte, error) {
	return p.pick().ReadBlob(ctx, digest)
}

func (p *Pool) ReaderForBlob(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	return p.pick().ReaderForBlob(ctx, digest)
}

// ReaderForBlobs serves the whole list from one member, so the batches of a
// single stream stay on one connection and arrive in order. Pooling still pays
// off across streams: concurrent readers land on different members.
func (p *Pool) ReaderForBlobs(ctx context.Context, digests []Digest) (io.ReadCloser, error) {
	return p.pick().ReaderForBlobs(ctx, digests)
}

// RetryStats reports what the retry loops of every member did.
func (p *Pool) RetryStats() RetryStats {
	total := RetryStats{ByCode: make(map[codes.Code]uint64)}
	for _, member := range p.members {
		reporter, ok := member.(interface{ RetryStats() RetryStats })
		if !ok {
			continue
		}
		stats := reporter.RetryStats()
		total.Retries += stats.Retries
		total.GaveUp += stats.GaveUp
		for code, n := range stats.ByCode {
			total.ByCode[code] += n
		}
	}
	return total
}
