package cas

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	bytestream_proto "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

type CAS struct {
	casClient        remoteexecution_proto.ContentAddressableStorageClient
	byteStreamClient bytestream_proto.ByteStreamClient
	capabilities     capabilities
	instanceName     string
	retry            retryConfig

	// peers are clients for the same remote cache on independent gRPC
	// connections (the members of a Pool). A retry hops to the next one, so a
	// connection the server poisoned is not immediately reused. Empty when this
	// client is not part of a pool.
	peers []*CAS
	// peerIndex is this client's own position in peers.
	peerIndex int
}

func New(clientConn *grpc.ClientConn, opts ...casOption) (*CAS, error) {
	casOpts := &casOptions{
		capabilities: capabilities{
			DigestFunctionSHA256:   true,
			MaxBatchTotalSizeBytes: 2 * 1024 * 1024, // 2 MiB
		},
		learnCapabilities: false,
		retryPolicy:       envRetryPolicy(),
	}
	for _, opt := range opts {
		opt(casOpts)
	}
	capabilities := casOpts.capabilities
	retry := newRetryConfig(casOpts.retryPolicy)

	casClient := remoteexecution_proto.NewContentAddressableStorageClient(clientConn)
	byteStreamClient := bytestream_proto.NewByteStreamClient(clientConn)

	if casOpts.learnCapabilities {
		capabilitiesClient := remoteexecution_proto.NewCapabilitiesClient(clientConn)
		var err error
		capabilities, err = learnCapabilities(context.Background(), capabilitiesClient, casOpts.instanceName, retry)
		if err != nil {
			return nil, fmt.Errorf("failed to learn capabilities: %w", err)
		}
		if !capabilities.DigestFunctionSHA256 {
			return nil, errors.New("REAPI does not support SHA256 digest function")
		}
	}

	return &CAS{
		casClient:        casClient,
		byteStreamClient: byteStreamClient,
		capabilities:     capabilities,
		instanceName:     casOpts.instanceName,
		retry:            retry,
	}, nil
}

// RetryStats reports what this client's retry loops did.
func (c *CAS) RetryStats() RetryStats {
	return c.retry.counters.snapshot()
}

// peer returns the client to use for the given attempt: this one for the first
// attempt, and a sibling connection from the pool (if any) for retries.
func (c *CAS) peer(attempt int) *CAS {
	if attempt == 0 || len(c.peers) < 2 {
		return c
	}
	return c.peers[(c.peerIndex+attempt)%len(c.peers)]
}

// callContext applies the per-attempt deadline for a unary call.
func (c *CAS) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.retry.policy.RPCTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.retry.policy.RPCTimeout)
}

func (c *CAS) FindMissingBlobs(ctx context.Context, digests []Digest) ([]Digest, error) {
	if len(digests) == 0 {
		return nil, nil // nothing to do
	}
	if !c.capabilities.supportedDigestFunction(digests[0].algorithm) {
		return nil, fmt.Errorf("unsupported digest algorithm: %s", digests[0].algorithm)
	}
	digestFunction := digests[0].protoDigestFunction()

	for _, d := range digests {
		if d.algorithm != digests[0].algorithm {
			return nil, fmt.Errorf("all digests must use the same algorithm: %s != %s", d.algorithm, digests[0].algorithm)
		}
	}
	var protoDigests []*remoteexecution_proto.Digest
	for _, d := range digests {
		protoDigests = append(protoDigests, d.protoDigest())
	}
	request := &remoteexecution_proto.FindMissingBlobsRequest{
		InstanceName:   c.instanceName,
		BlobDigests:    protoDigests,
		DigestFunction: digestFunction,
	}

	r := c.retry.start(fmt.Sprintf("looking up %d blob(s) in the remote cache", len(digests)))
	for {
		resp, err := c.peer(r.attempt).findMissingBlobsOnce(ctx, request)
		if err == nil {
			return missingFromResponse(resp, digestFunction)
		}
		if giveUp := r.next(ctx, err); giveUp != nil {
			return nil, casErr(giveUp)
		}
	}
}

func (c *CAS) findMissingBlobsOnce(ctx context.Context, request *remoteexecution_proto.FindMissingBlobsRequest) (*remoteexecution_proto.FindMissingBlobsResponse, error) {
	ctx, cancel := c.callContext(ctx)
	defer cancel()
	return c.casClient.FindMissingBlobs(ctx, request)
}

func missingFromResponse(resp *remoteexecution_proto.FindMissingBlobsResponse, digestFunction remoteexecution_proto.DigestFunction_Value) ([]Digest, error) {
	if len(resp.MissingBlobDigests) == 0 {
		return nil, nil // no missing blobs
	}
	var missing []Digest
	for _, d := range resp.MissingBlobDigests {
		digest, err := DigestFromProto(d, digestFunction)
		if err != nil {
			return nil, fmt.Errorf("failed to convert proto digest: %w", err)
		}
		missing = append(missing, digest)
	}
	return missing, nil
}

func (c *CAS) ReadBlob(ctx context.Context, digest Digest) ([]byte, error) {
	if !c.capabilities.supportedDigestFunction(digest.algorithm) {
		return nil, fmt.Errorf("unsupported digest algorithm: %s", digest.algorithm)
	}
	if digest.SizeBytes == 0 {
		return nil, nil // blob is empty
	}
	if digest.SizeBytes <= c.capabilities.MaxBatchTotalSizeBytes {
		// If the blob is small enough, we can use BatchReadBlobs.
		return c.batchReadOne(ctx, digest)
	}
	// For larger blobs, we use ByteStream to read the blob in chunks.
	stream, err := c.streamReadOne(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, stream); err != nil {
		return nil, fmt.Errorf("failed to read blob: %w", err)
	}
	return buf.Bytes(), nil
}

func (c *CAS) ReaderForBlob(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	if !c.capabilities.supportedDigestFunction(digest.algorithm) {
		return nil, fmt.Errorf("unsupported digest algorithm: %s", digest.algorithm)
	}
	if digest.SizeBytes == 0 {
		return io.NopCloser(bytes.NewReader(nil)), nil // blob is empty
	}
	if digest.SizeBytes <= c.capabilities.MaxBatchTotalSizeBytes {
		// If the blob is small enough, we can use BatchReadBlobs.
		data, err := c.batchReadOne(ctx, digest)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	// For larger blobs, we use ByteStream to read the blob in chunks.
	return c.streamReadOne(ctx, digest)
}

func (c *CAS) batchReadOne(ctx context.Context, digest Digest) ([]byte, error) {
	blobs, err := c.batchRead(ctx, []Digest{digest})
	if err != nil {
		return nil, err
	}
	return blobs[0], nil
}

func (c *CAS) streamReadOne(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	resourceName := fmt.Sprintf("blobs/%x/%d", digest.Hash, digest.SizeBytes)
	if c.instanceName != "" {
		resourceName = c.instanceName + "/" + resourceName
	}
	r := &byteStreamReadCloser{
		owner:        c,
		ctx:          ctx,
		resourceName: resourceName,
		limit:        digest.SizeBytes,
		idleTimeout:  c.retry.policy.IdleTimeout,
		retrier:      c.retry.start(fmt.Sprintf("streaming blob %s (%d bytes) from the remote cache", digest.hexHash(), digest.SizeBytes)),
	}
	if err := r.ensureStream(); err != nil {
		return nil, fmt.Errorf("failed to read blob: %w", err)
	}
	return r, nil
}

type Digest struct {
	algorithm string
	Hash      []byte
	SizeBytes int64
}

func SHA256(hash []byte, sizeBytes int64) Digest {
	return Digest{
		algorithm: "sha256",
		Hash:      hash,
		SizeBytes: sizeBytes,
	}
}

func SHA512(hash []byte, sizeBytes int64) Digest {
	return Digest{
		algorithm: "sha512",
		Hash:      hash,
		SizeBytes: sizeBytes,
	}
}

func DigestFromProto(digest *remoteexecution_proto.Digest, digestFunction remoteexecution_proto.DigestFunction_Value) (Digest, error) {
	hash, err := hex.DecodeString(digest.Hash)
	if err != nil {
		return Digest{}, fmt.Errorf("failed to decode digest hash: %w", err)
	}
	switch digestFunction {
	case remoteexecution_proto.DigestFunction_SHA256:
		return SHA256(hash, digest.SizeBytes), nil
	case remoteexecution_proto.DigestFunction_SHA512:
		return SHA512(hash, digest.SizeBytes), nil
	}
	return Digest{}, fmt.Errorf("unsupported digest function: %s", digestFunction)
}

func (d Digest) protoDigest() *remoteexecution_proto.Digest {
	return &remoteexecution_proto.Digest{
		Hash:      fmt.Sprintf("%x", d.Hash),
		SizeBytes: d.SizeBytes,
	}
}

func (d Digest) protoDigestFunction() remoteexecution_proto.DigestFunction_Value {
	switch d.algorithm {
	case "sha256":
		return remoteexecution_proto.DigestFunction_SHA256
	case "sha512":
		return remoteexecution_proto.DigestFunction_SHA512
	default:
		return remoteexecution_proto.DigestFunction_UNKNOWN
	}
}

// max_batch_total_size_bytes limits blob payload, not the serialized message: the spec
// calls it the "maximum total size of blobs" and names the transport's message limit
// separately, and Buildbarn enforces it that way, counting only digest sizes. So a
// request for exactly the declared limit is inside the server's stated budget and still
// 103 bytes (BatchUpdateBlobsRequest) or 157 bytes (ByteStream WriteRequest) over what
// gRPC will carry. The limit that binds is the one on what a *server* receives, which
// nothing a client sets can raise, hence the headroom below.
const (
	// maxBatchMessageSize is what a server is assumed to receive when it declares more
	// than this, or nothing: grpc-go's defaultServerMaxReceiveMessageSize, and
	// grpc-java's.
	maxBatchMessageSize = 4 * 1024 * 1024

	// batchFramingHeadroom covers the per-blob digest, status and length-delimiter
	// fields, and a WriteRequest's resource name. Same figure as
	// bazelbuild/remote-apis-sdks holds back in DefaultMaxBatchSize (4*1024*1024 - 1024).
	batchFramingHeadroom = 1024

	// minBatchPayload keeps an unusually small declared limit from driving the budget to
	// zero. It also sizes WriteBlob's chunk buffer, which at zero makes no progress.
	minBatchPayload = 64 * 1024
)

type capabilities struct {
	DigestFunctionSHA256   bool
	DigestFunctionSHA512   bool
	MaxBatchTotalSizeBytes int64
}

func (c capabilities) supportedDigestFunction(algorithm string) bool {
	switch algorithm {
	case "sha256":
		return c.DigestFunctionSHA256
	case "sha512":
		return c.DigestFunctionSHA512
	}
	return false
}

func learnCapabilities(ctx context.Context, capabilitiesClient remoteexecution_proto.CapabilitiesClient, instanceName string, retry retryConfig) (capabilities, error) {
	request := &remoteexecution_proto.GetCapabilitiesRequest{InstanceName: instanceName}
	r := retry.start("asking the remote cache for its capabilities")
	var resp *remoteexecution_proto.ServerCapabilities
	for {
		var err error
		resp, err = getCapabilitiesOnce(ctx, capabilitiesClient, request, retry.policy)
		if err == nil {
			break
		}
		if giveUp := r.next(ctx, err); giveUp != nil {
			return capabilities{}, casErr(giveUp)
		}
	}
	if resp == nil {
		return capabilities{}, errors.New("capabilities response is nil")
	}
	if resp.CacheCapabilities == nil {
		return capabilities{}, errors.New("capabilities response has no cache capabilities")
	}

	var caps capabilities
	for _, f := range resp.CacheCapabilities.DigestFunctions {
		if f == remoteexecution_proto.DigestFunction_SHA256 {
			caps.DigestFunctionSHA256 = true
		}
		if f == remoteexecution_proto.DigestFunction_SHA512 {
			caps.DigestFunctionSHA512 = true
		}
	}
	declared := resp.CacheCapabilities.MaxBatchTotalSizeBytes
	if declared <= 0 {
		// Assume 1 MiB when the server declares no limit.
		declared = 1 * 1024 * 1024
	}
	caps.MaxBatchTotalSizeBytes = batchPayloadBudget(declared)
	return caps, nil
}

// batchPayloadBudget turns a batch message limit into how much blob payload may go in
// one. Headroom comes off whichever limit binds, the server's or ours: a server declaring
// under 4 MiB may be enforcing a lower transport limit to match.
func batchPayloadBudget(messageLimit int64) int64 {
	budget := min(messageLimit, int64(maxBatchMessageSize)) - batchFramingHeadroom
	if budget < minBatchPayload {
		return minBatchPayload
	}
	return budget
}

func getCapabilitiesOnce(ctx context.Context, client remoteexecution_proto.CapabilitiesClient, request *remoteexecution_proto.GetCapabilitiesRequest, policy RetryPolicy) (*remoteexecution_proto.ServerCapabilities, error) {
	if policy.RPCTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, policy.RPCTimeout)
		defer cancel()
	}
	return client.GetCapabilities(ctx, request)
}

type byteStreamReadCloser struct {
	// Fields needed to (re)establish the underlying stream so a read that the
	// server tears down mid-transfer can be resumed from the current offset.
	owner        *CAS
	ctx          context.Context
	resourceName string
	idleTimeout  time.Duration
	retrier      *retrier

	stream bytestream_proto.ByteStream_ReadClient
	buf    bytes.Buffer
	eof    bool
	cancel context.CancelFunc

	limit          int64
	readFromRemote int64
	writtenToOut   int64
}

// ensureStream opens the ByteStream read at the current offset, retrying
// transient failures. On reconnect the offset is the number of bytes already
// received from the server, so the server resumes exactly where it left off and
// nothing is transferred twice.
func (b *byteStreamReadCloser) ensureStream() error {
	for {
		err := b.connect(b.readFromRemote)
		if err == nil {
			return nil
		}
		if giveUp := b.retrier.next(b.ctx, err); giveUp != nil {
			return casErr(giveUp)
		}
	}
}

// connect (re)opens the ByteStream read at the given offset, cancelling any
// previous stream first. Retries use a sibling connection from the pool when
// there is one.
func (b *byteStreamReadCloser) connect(offset int64) error {
	b.closeStream()
	ctx, cancel := context.WithCancel(b.ctx)
	stream, err := b.owner.peer(b.retrier.attempt).byteStreamClient.Read(ctx, &bytestream_proto.ReadRequest{
		ResourceName: b.resourceName,
		ReadOffset:   offset,
	})
	if err != nil {
		cancel()
		return err
	}
	if stream == nil {
		cancel()
		return errors.New("byte stream response is nil")
	}
	b.stream = stream
	b.cancel = cancel
	return nil
}

// closeStream tears down the current stream, if any.
func (b *byteStreamReadCloser) closeStream() {
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	b.stream = nil
}

// recvWithReconnect wraps stream.Recv, transparently reconnecting and resuming
// from the current offset when the server tears the stream down mid-transfer
// (e.g. an HTTP/2 RST_STREAM after an idle period) or stops sending data
// altogether. It gives up after RetryPolicy.MaxAttempts consecutive failures
// without forward progress.
func (b *byteStreamReadCloser) recvWithReconnect() (*bytestream_proto.ReadResponse, error) {
	// If the whole blob has already been received, we're done. Avoid an extra
	// Recv/reconnect that could hit OUT_OF_RANGE at the tail if the stream was
	// torn down right after the final byte.
	if b.readFromRemote >= b.limit {
		return nil, io.EOF
	}
	for {
		resp, err := b.recvOnce()
		if err == nil {
			if len(resp.GetData()) > 0 {
				// Forward progress: restore the full retry budget.
				b.retrier.progress()
			}
			return resp, nil
		}
		if err == io.EOF {
			return resp, io.EOF
		}
		if giveUp := b.retrier.next(b.ctx, err); giveUp != nil {
			return nil, casErr(giveUp)
		}
		if err := b.ensureStream(); err != nil {
			return nil, err
		}
	}
}

// recvOnce receives one chunk, bounding how long the server may go without
// sending anything. A whole-call deadline cannot tell a slow transfer from a
// hung one, so a stalled read is cancelled and reported as a deadline, which the
// retry loop resumes from the current offset (cf. Bazel's
// --remote_grpc_download_idle_timeout).
func (b *byteStreamReadCloser) recvOnce() (*bytestream_proto.ReadResponse, error) {
	if b.idleTimeout <= 0 {
		return b.stream.Recv()
	}
	// Capture the cancel func: the watchdog must only ever cancel the stream it
	// was armed for.
	cancel := b.cancel
	var stalled atomic.Bool
	watchdog := time.AfterFunc(b.idleTimeout, func() {
		stalled.Store(true)
		cancel()
	})
	defer watchdog.Stop()

	resp, err := b.stream.Recv()
	if err != nil && stalled.Load() && b.ctx.Err() == nil {
		return nil, status.Errorf(codes.DeadlineExceeded,
			"no data received for %v at offset %d", b.idleTimeout, b.readFromRemote)
	}
	return resp, err
}

func (b *byteStreamReadCloser) Read(p []byte) (n int, err error) {
	// first, check if we have data from the previous read
	budget := len(p)
	availableFromLastRead := b.buf.Len()
	copyFromLastRead := min(budget, availableFromLastRead)
	if copyFromLastRead > 0 {
		n := copy(p, b.buf.Next(copyFromLastRead))
		if n > budget {
			// should never happen
			panic(fmt.Sprintf("copy(%d, %d) > %d (budget exceeded)", n, copyFromLastRead, budget))
		}
		if n != copyFromLastRead {
			// should never happen
			panic(fmt.Sprintf("copy(%d, %d) != %d (logic flaw)", n, copyFromLastRead, n))
		}
		b.writtenToOut += int64(n)
		budget -= n
	}
	if budget == 0 {
		// we can fulfill the request with buffered data
		return len(p), b.nilOrEOF()
	}
	// buffer was drained

	if b.eof {
		// we are at the end of the stream
		// and drained the buffer
		// the reader is done
		return 0, io.EOF
	}

	// read from the stream, transparently reconnecting and resuming from the
	// current offset if the server tears the stream down mid-transfer.
	resp, err := b.recvWithReconnect()
	var readFromRemoteNow int
	if resp != nil {
		readFromRemoteNow = len(resp.Data)
	}
	if err == io.EOF {
		// we are at the end of the stream
		// we will also not call Recv again
		// we will return EOF after the buffer is drained
		b.eof = true
	} else if err != nil {
		// already wrapped by recvWithReconnect/connect
		return 0, err
	}
	b.readFromRemote += int64(readFromRemoteNow)

	// copy the data to the buffer
	n = 0
	if resp != nil {
		n = copy(p[copyFromLastRead:], resp.Data)
	}
	b.writtenToOut += int64(n)
	if n < readFromRemoteNow {
		// we have more data than the requested read wants
		// buffer for next call
		b.buf.Write(resp.Data[n:])
	}
	copiedToOutTotal := copyFromLastRead + n
	return copiedToOutTotal, b.nilOrEOF()
}

func (b *byteStreamReadCloser) Close() error {
	// cancel the context to
	// stop the stream from our side
	b.closeStream()
	return nil
}

func (b *byteStreamReadCloser) nilOrEOF() error {
	if b.eof && b.buf.Len() == 0 {
		return io.EOF
	}
	return nil
}

type casOptions struct {
	capabilities      capabilities
	learnCapabilities bool
	instanceName      string
	retryPolicy       RetryPolicy
}

type casOption func(*casOptions)

// WithRetryPolicy sets how transient remote cache failures are retried. The
// default is [RetryPolicyFromEnv].
func WithRetryPolicy(policy RetryPolicy) casOption {
	return func(opts *casOptions) {
		opts.retryPolicy = policy
	}
}

func WithLearnCapabilities(learn bool) casOption {
	return func(opts *casOptions) {
		opts.learnCapabilities = learn
	}
}

func WithMaxBatchTotalSizeBytes(maxBatchTotalSizeBytes int64) casOption {
	return func(opts *casOptions) {
		opts.capabilities.MaxBatchTotalSizeBytes = maxBatchTotalSizeBytes
	}
}

func WithSHA256(supprted bool) casOption {
	return func(opts *casOptions) {
		opts.capabilities.DigestFunctionSHA256 = supprted
	}
}

func WithSHA512(supported bool) casOption {
	return func(opts *casOptions) {
		opts.capabilities.DigestFunctionSHA512 = supported
	}
}

func WithInstanceName(instanceName string) casOption {
	return func(opts *casOptions) {
		opts.instanceName = instanceName
	}
}
