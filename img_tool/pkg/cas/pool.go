package cas

import (
	"context"
	"io"
	"sync/atomic"
)

// blobSource is the subset of *CAS a Pool distributes reads across. It exists
// so the pool's round-robin logic can be unit-tested with fakes.
type blobSource interface {
	FindMissingBlobs(ctx context.Context, digests []Digest) ([]Digest, error)
	ReadBlob(ctx context.Context, digest Digest) ([]byte, error)
	ReaderForBlob(ctx context.Context, digest Digest) (io.ReadCloser, error)
}

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
	members []blobSource
	next    atomic.Uint64
}

// NewPool returns a Pool that round-robins reads across the given CAS members.
// Each member should be backed by its own gRPC connection for pooling to have
// any effect. NewPool panics if members is empty; a single-member pool is
// valid and behaves like the member itself.
func NewPool(members []*CAS) *Pool {
	sources := make([]blobSource, len(members))
	for i, m := range members {
		sources[i] = m
	}
	return newPool(sources)
}

func newPool(members []blobSource) *Pool {
	if len(members) == 0 {
		panic("cas.NewPool: at least one member is required")
	}
	return &Pool{members: members}
}

// pick returns the next member in round-robin order.
func (p *Pool) pick() blobSource {
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
