package cas

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	bytestream_proto "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"

	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

type CAS struct {
	casClient        remoteexecution_proto.ContentAddressableStorageClient
	byteStreamClient bytestream_proto.ByteStreamClient
	capabilities     capabilities
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
		capabilities, err = learnCapabilities(context.Background(), capabilitiesClient)
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
	defer func() { _ = stream.Close() }()
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
	ctx, cancel := context.WithCancel(ctx)
	resp, err := c.byteStreamClient.Read(ctx, &bytestream_proto.ReadRequest{
		ResourceName: fmt.Sprintf("blobs/%x/%d", digest.Hash, digest.SizeBytes),
		ReadOffset:   0,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to read blob: %w", casErr(err))
	}
	if resp == nil {
		cancel()
		return nil, errors.New("byte stream response is nil")
	}
	return &byteStreamReadCloser{
		stream: resp,
		cancel: cancel,
		limit:  digest.SizeBytes,
	}, nil
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

func learnCapabilities(ctx context.Context, capabilitiesClient remoteexecution_proto.CapabilitiesClient) (capabilities, error) {
	resp, err := capabilitiesClient.GetCapabilities(ctx, &remoteexecution_proto.GetCapabilitiesRequest{})
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
	stream bytestream_proto.ByteStream_ReadClient
	buf    bytes.Buffer
	eof    bool
	cancel context.CancelFunc

	limit          int64
	readFromRemote int64
	writtenToOut   int64
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

	// read from the stream
	resp, err := b.stream.Recv()
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
		return 0, casErr(err)
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
	b.cancel()
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
