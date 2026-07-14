package cas

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	bytestream_proto "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

const (
	// maxByteStreamReconnects bounds how many times we transparently reconnect a
	// ByteStream read that the server tore down mid-transfer before giving up.
	// The counter resets whenever we make forward progress, so this only limits
	// consecutive failures without any data.
	maxByteStreamReconnects = 5
)

// Backoff between ByteStream reconnect attempts. Declared as vars so tests can
// shrink them.
var (
	byteStreamBaseBackoff = 250 * time.Millisecond
	byteStreamMaxBackoff  = 5 * time.Second
)

type CAS struct {
	casClient        remoteexecution_proto.ContentAddressableStorageClient
	byteStreamClient bytestream_proto.ByteStreamClient
	capabilities     capabilities
	instanceName     string
}

func New(clientConn *grpc.ClientConn, opts ...casOption) (*CAS, error) {
	casOpts := &casOptions{
		capabilities: capabilities{
			DigestFunctionSHA256:   true,
			MaxBatchTotalSizeBytes: 2 * 1024 * 1024, // 2 MiB
		},
		learnCapabilities: false,
	}
	for _, opt := range opts {
		opt(casOpts)
	}
	capabilities := casOpts.capabilities

	casClient := remoteexecution_proto.NewContentAddressableStorageClient(clientConn)
	byteStreamClient := bytestream_proto.NewByteStreamClient(clientConn)

	if casOpts.learnCapabilities {
		capabilitiesClient := remoteexecution_proto.NewCapabilitiesClient(clientConn)
		var err error
		capabilities, err = learnCapabilities(context.Background(), capabilitiesClient, casOpts.instanceName)
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
	}, nil
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
	resp, err := c.casClient.FindMissingBlobs(ctx, &remoteexecution_proto.FindMissingBlobsRequest{
		InstanceName:   c.instanceName,
		BlobDigests:    protoDigests,
		DigestFunction: digestFunction,
	})
	if err != nil {
		return nil, casErr(err)
	}
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
	resp, err := c.casClient.BatchReadBlobs(ctx, &remoteexecution_proto.BatchReadBlobsRequest{
		InstanceName:   c.instanceName,
		Digests:        []*remoteexecution_proto.Digest{digest.protoDigest()},
		DigestFunction: digest.protoDigestFunction(),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to read blob: %w", casErr(err))
	}
	if len(resp.Responses) != 1 {
		return nil, errors.New("unexpected number of responses from BatchReadBlobs")
	}
	if resp.Responses[0].Status != nil && resp.Responses[0].Status.Code != 0 {
		return nil, fmt.Errorf("failed to read blob: %s", resp.Responses[0].Status.String())
	}
	if len(resp.Responses[0].Data) != int(digest.SizeBytes) {
		return nil, fmt.Errorf("unexpected size of blob data: got %d bytes, expected %d bytes", len(resp.Responses[0].Data), digest.SizeBytes)
	}
	return resp.Responses[0].Data, nil
}

func (c *CAS) streamReadOne(ctx context.Context, digest Digest) (io.ReadCloser, error) {
	resourceName := fmt.Sprintf("blobs/%x/%d", digest.Hash, digest.SizeBytes)
	if c.instanceName != "" {
		resourceName = c.instanceName + "/" + resourceName
	}
	r := &byteStreamReadCloser{
		client:       c.byteStreamClient,
		ctx:          ctx,
		resourceName: resourceName,
		limit:        digest.SizeBytes,
	}
	if err := r.connect(0); err != nil {
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

func learnCapabilities(ctx context.Context, capabilitiesClient remoteexecution_proto.CapabilitiesClient, instanceName string) (capabilities, error) {
	resp, err := capabilitiesClient.GetCapabilities(ctx, &remoteexecution_proto.GetCapabilitiesRequest{
		InstanceName: instanceName,
	})
	if err != nil {
		return capabilities{}, casErr(err)
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
	caps.MaxBatchTotalSizeBytes = resp.CacheCapabilities.MaxBatchTotalSizeBytes
	if caps.MaxBatchTotalSizeBytes <= 0 {
		// Default to 1 MiB if not set.
		caps.MaxBatchTotalSizeBytes = 1 * 1024 * 1024
	}
	if caps.MaxBatchTotalSizeBytes > 4*1024*1024 {
		// Cap to 4 MiB to avoid excessive memory usage.
		caps.MaxBatchTotalSizeBytes = 4 * 1024 * 1024
	}
	return caps, nil
}

type byteStreamReadCloser struct {
	// Fields needed to (re)establish the underlying stream so a read that the
	// server tears down mid-transfer can be resumed from the current offset.
	client       bytestream_proto.ByteStreamClient
	ctx          context.Context
	resourceName string

	stream bytestream_proto.ByteStream_ReadClient
	buf    bytes.Buffer
	eof    bool
	cancel context.CancelFunc

	limit             int64
	readFromRemote    int64
	writtenToOut      int64
	reconnectAttempts int
}

// connect (re)opens the ByteStream read at the given offset, cancelling any
// previous stream first. On reconnect, offset is the number of bytes already
// received from the server, so the server resumes exactly where it left off.
func (b *byteStreamReadCloser) connect(offset int64) error {
	if b.cancel != nil {
		b.cancel()
	}
	ctx, cancel := context.WithCancel(b.ctx)
	stream, err := b.client.Read(ctx, &bytestream_proto.ReadRequest{
		ResourceName: b.resourceName,
		ReadOffset:   offset,
	})
	if err != nil {
		cancel()
		return casErr(err)
	}
	if stream == nil {
		cancel()
		return errors.New("byte stream response is nil")
	}
	b.stream = stream
	b.cancel = cancel
	return nil
}

// recvWithReconnect wraps stream.Recv, transparently reconnecting and resuming
// from the current offset when the server tears the stream down mid-transfer
// (e.g. an HTTP/2 RST_STREAM after an idle period). It gives up after
// maxByteStreamReconnects consecutive failures without forward progress.
func (b *byteStreamReadCloser) recvWithReconnect() (*bytestream_proto.ReadResponse, error) {
	// If the whole blob has already been received, we're done. Avoid an extra
	// Recv/reconnect that could hit OUT_OF_RANGE at the tail if the stream was
	// torn down right after the final byte.
	if b.readFromRemote >= b.limit {
		return nil, io.EOF
	}
	for {
		resp, err := b.stream.Recv()
		if err == nil {
			if len(resp.GetData()) > 0 {
				// Forward progress: restore the full reconnect budget.
				b.reconnectAttempts = 0
			}
			return resp, nil
		}
		if err == io.EOF {
			return resp, io.EOF
		}
		if !b.shouldReconnect(err) {
			return nil, casErr(err)
		}
		b.reconnectAttempts++
		fmt.Fprintf(os.Stderr,
			"WARNING: CAS byte stream read of %q interrupted at offset %d: %v; reconnecting to resume (attempt %d/%d)\n",
			b.resourceName, b.readFromRemote, err, b.reconnectAttempts, maxByteStreamReconnects)
		if err := b.sleepBackoff(); err != nil {
			return nil, casErr(err)
		}
		if err := b.connect(b.readFromRemote); err != nil {
			return nil, err
		}
	}
}

// shouldReconnect reports whether a Recv error is a transient server-side
// stream teardown we can recover from by resuming the read.
func (b *byteStreamReadCloser) shouldReconnect(err error) bool {
	if b.reconnectAttempts >= maxByteStreamReconnects {
		return false
	}
	// Our own cancellation or deadline (e.g. Close, or a caller-imposed timeout)
	// is not a transient server failure — don't try to resume.
	if b.ctx.Err() != nil {
		return false
	}
	switch status.Code(err) {
	case codes.Unavailable, codes.Internal, codes.Aborted:
		return true
	default:
		return false
	}
}

// sleepBackoff waits before the next reconnect, honoring context cancellation.
func (b *byteStreamReadCloser) sleepBackoff() error {
	backoff := byteStreamBaseBackoff * time.Duration(1<<(b.reconnectAttempts-1))
	if backoff > byteStreamMaxBackoff {
		backoff = byteStreamMaxBackoff
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-b.ctx.Done():
		return b.ctx.Err()
	case <-timer.C:
		return nil
	}
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
	if b.cancel != nil {
		b.cancel()
	}
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
}

type casOption func(*casOptions)

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
