package cas

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	bytestream_proto "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

// fakeCASClient answers the unary CAS RPCs. Each RPC fails with failErr for the
// first failTimes calls and succeeds afterwards, so tests can watch the retry
// loop work through a transient outage.
type fakeCASClient struct {
	blob      []byte
	failErr   error
	failTimes int
	// perBlobStatus fails the single blob inside the batch response rather than
	// the call itself.
	perBlobStatus bool

	mu    sync.Mutex
	calls int
}

func (f *fakeCASClient) attempt() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failTimes {
		return f.failErr
	}
	return nil
}

func (f *fakeCASClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeCASClient) FindMissingBlobs(context.Context, *remoteexecution_proto.FindMissingBlobsRequest, ...grpc.CallOption) (*remoteexecution_proto.FindMissingBlobsResponse, error) {
	if err := f.attempt(); err != nil {
		return nil, err
	}
	return &remoteexecution_proto.FindMissingBlobsResponse{}, nil
}

func (f *fakeCASClient) BatchUpdateBlobs(_ context.Context, in *remoteexecution_proto.BatchUpdateBlobsRequest, _ ...grpc.CallOption) (*remoteexecution_proto.BatchUpdateBlobsResponse, error) {
	err := f.attempt()
	if err != nil && !f.perBlobStatus {
		return nil, err
	}
	response := &remoteexecution_proto.BatchUpdateBlobsResponse_Response{Digest: in.Requests[0].Digest}
	if err != nil {
		response.Status = status.Convert(err).Proto()
	}
	return &remoteexecution_proto.BatchUpdateBlobsResponse{
		Responses: []*remoteexecution_proto.BatchUpdateBlobsResponse_Response{response},
	}, nil
}

func (f *fakeCASClient) BatchReadBlobs(_ context.Context, in *remoteexecution_proto.BatchReadBlobsRequest, _ ...grpc.CallOption) (*remoteexecution_proto.BatchReadBlobsResponse, error) {
	err := f.attempt()
	if err != nil && !f.perBlobStatus {
		return nil, err
	}
	response := &remoteexecution_proto.BatchReadBlobsResponse_Response{Digest: in.Digests[0]}
	if err != nil {
		response.Status = status.Convert(err).Proto()
	} else {
		response.Data = f.blob
	}
	return &remoteexecution_proto.BatchReadBlobsResponse{
		Responses: []*remoteexecution_proto.BatchReadBlobsResponse_Response{response},
	}, nil
}

func (f *fakeCASClient) GetTree(context.Context, *remoteexecution_proto.GetTreeRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[remoteexecution_proto.GetTreeResponse], error) {
	panic("not implemented")
}

func (f *fakeCASClient) SplitBlob(context.Context, *remoteexecution_proto.SplitBlobRequest, ...grpc.CallOption) (*remoteexecution_proto.SplitBlobResponse, error) {
	panic("not implemented")
}

func (f *fakeCASClient) SpliceBlob(context.Context, *remoteexecution_proto.SpliceBlobRequest, ...grpc.CallOption) (*remoteexecution_proto.SpliceBlobResponse, error) {
	panic("not implemented")
}

func unavailable() error { return status.Error(codes.Unavailable, "unavailable") }

func TestBatchReadRetriesTransientFailure(t *testing.T) {
	blob := testBlob(100)
	fake := &fakeCASClient{blob: blob, failErr: unavailable(), failTimes: 2}
	c := testCAS(nil, fake)

	got, err := c.ReadBlob(context.Background(), SHA256(make([]byte, 32), int64(len(blob))))
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if len(got) != len(blob) {
		t.Fatalf("read %d bytes, want %d", len(got), len(blob))
	}
	if fake.callCount() != 3 {
		t.Fatalf("BatchReadBlobs calls = %d, want 3", fake.callCount())
	}
	if stats := c.RetryStats(); stats.Retries != 2 || stats.ByCode[codes.Unavailable] != 2 {
		t.Fatalf("retry stats = %+v, want 2 retries on unavailable", stats)
	}
}

func TestBatchReadRetriesTransientPerBlobStatus(t *testing.T) {
	blob := testBlob(100)
	// BatchReadBlobs reports per-blob outcomes in the response, so a transient
	// failure can arrive that way too.
	fake := &fakeCASClient{blob: blob, failErr: unavailable(), failTimes: 1, perBlobStatus: true}
	c := testCAS(nil, fake)

	if _, err := c.ReadBlob(context.Background(), SHA256(make([]byte, 32), int64(len(blob)))); err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if fake.callCount() != 2 {
		t.Fatalf("BatchReadBlobs calls = %d, want 2", fake.callCount())
	}
}

func TestBatchReadDoesNotRetryNotFound(t *testing.T) {
	fake := &fakeCASClient{failErr: status.Error(codes.NotFound, "no such blob"), failTimes: 99}
	c := testCAS(nil, fake)

	_, err := c.ReadBlob(context.Background(), SHA256(make([]byte, 32), 100))
	if status.Code(err) != codes.NotFound {
		t.Fatalf("error = %v, want NotFound", err)
	}
	if fake.callCount() != 1 {
		t.Fatalf("BatchReadBlobs calls = %d, want 1 (a cache miss is not retried)", fake.callCount())
	}
}

func TestBatchReadGivesUpAfterMaxAttempts(t *testing.T) {
	fake := &fakeCASClient{failErr: unavailable(), failTimes: 99}
	c := testCAS(nil, fake)

	_, err := c.ReadBlob(context.Background(), SHA256(make([]byte, 32), 100))
	if err == nil {
		t.Fatal("expected an error after exhausting the attempt budget")
	}
	if want := testRetryPolicy().MaxAttempts; fake.callCount() != want {
		t.Fatalf("BatchReadBlobs calls = %d, want %d", fake.callCount(), want)
	}
	// The error says how hard we tried, so a give-up is distinguishable from a
	// first-attempt failure.
	if !strings.Contains(err.Error(), "giving up") {
		t.Fatalf("error = %v, want it to mention giving up", err)
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("status code = %s, want %s", status.Code(err), codes.Unavailable)
	}
	if stats := c.RetryStats(); stats.GaveUp != 1 {
		t.Fatalf("retry stats = %+v, want one give-up", stats)
	}
}

func TestFindMissingBlobsRetriesTransientFailure(t *testing.T) {
	fake := &fakeCASClient{failErr: unavailable(), failTimes: 1}
	c := testCAS(nil, fake)

	if _, err := c.FindMissingBlobs(context.Background(), []Digest{SHA256(make([]byte, 32), 100)}); err != nil {
		t.Fatalf("FindMissingBlobs: %v", err)
	}
	if fake.callCount() != 2 {
		t.Fatalf("FindMissingBlobs calls = %d, want 2", fake.callCount())
	}
}

func TestRetryHonorsServerRequestedDelay(t *testing.T) {
	detailed, err := status.New(codes.ResourceExhausted, "slow down").WithDetails(
		&errdetails.RetryInfo{RetryDelay: durationpb.New(40 * time.Millisecond)},
	)
	if err != nil {
		t.Fatalf("building status: %v", err)
	}
	fake := &fakeCASClient{blob: testBlob(10), failErr: detailed.Err(), failTimes: 1}
	c := testCAS(nil, fake)
	// The policy's own backoff is 1ms, so anything near the requested delay can
	// only have come from the RetryInfo detail.
	policy := testRetryPolicy()
	policy.MaxDelay = time.Second
	c.retry = newRetryConfig(policy)

	start := time.Now()
	if _, err := c.ReadBlob(context.Background(), SHA256(make([]byte, 32), 10)); err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if waited := time.Since(start); waited < 30*time.Millisecond {
		t.Fatalf("waited %v before retrying, want at least the requested 40ms", waited)
	}
}

func TestRetryDoesNotRetryLocalErrors(t *testing.T) {
	// A response that is short is not a transient server failure: retrying it
	// cannot help, and the error is not a gRPC status.
	fake := &fakeCASClient{blob: testBlob(10)}
	c := testCAS(nil, fake)

	if _, err := c.ReadBlob(context.Background(), SHA256(make([]byte, 32), 100)); err == nil {
		t.Fatal("expected a size mismatch error")
	}
	if fake.callCount() != 1 {
		t.Fatalf("BatchReadBlobs calls = %d, want 1", fake.callCount())
	}
}

func TestPoolRetriesOnAnotherConnection(t *testing.T) {
	blob := testBlob(100)
	// The first member is permanently unavailable; the second serves the blob.
	broken := &fakeCASClient{failErr: unavailable(), failTimes: 99}
	healthy := &fakeCASClient{blob: blob}
	first, second := testCAS(nil, broken), testCAS(nil, healthy)
	pool := NewPool([]*CAS{first, second})

	got, err := pool.ReadBlob(context.Background(), SHA256(make([]byte, 32), int64(len(blob))))
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	if len(got) != len(blob) {
		t.Fatalf("read %d bytes, want %d", len(got), len(blob))
	}
	if broken.callCount() != 1 || healthy.callCount() != 1 {
		t.Fatalf("calls: broken=%d healthy=%d, want 1 each (the retry hops connections)",
			broken.callCount(), healthy.callCount())
	}
	if stats := pool.RetryStats(); stats.Retries != 1 {
		t.Fatalf("pool retry stats = %+v, want one retry", stats)
	}
}

// stallingByteStreamClient delivers some bytes and then blocks forever, so the
// idle watchdog is the only thing that can end the read.
type stallingByteStreamClient struct {
	blob       []byte
	stallAfter int

	mu      sync.Mutex
	conns   int
	offsets []int64
}

func (f *stallingByteStreamClient) Read(ctx context.Context, in *bytestream_proto.ReadRequest, _ ...grpc.CallOption) (bytestream_proto.ByteStream_ReadClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	stall := f.conns == 0 // only the first connection stalls
	f.conns++
	f.offsets = append(f.offsets, in.ReadOffset)
	return &stallingReadClient{
		ctx:        ctx,
		data:       f.blob[in.ReadOffset:],
		chunkSize:  64,
		stallAfter: f.stallAfter,
		stall:      stall,
	}, nil
}

func (f *stallingByteStreamClient) Write(context.Context, ...grpc.CallOption) (bytestream_proto.ByteStream_WriteClient, error) {
	panic("not implemented")
}

func (f *stallingByteStreamClient) QueryWriteStatus(context.Context, *bytestream_proto.QueryWriteStatusRequest, ...grpc.CallOption) (*bytestream_proto.QueryWriteStatusResponse, error) {
	panic("not implemented")
}

type stallingReadClient struct {
	ctx        context.Context
	data       []byte
	chunkSize  int
	stallAfter int
	stall      bool

	pos  int
	sent int
}

func (r *stallingReadClient) Recv() (*bytestream_proto.ReadResponse, error) {
	if r.stall && r.sent >= r.stallAfter {
		// Block until the read is cancelled, which is what a hung transfer looks
		// like from here.
		<-r.ctx.Done()
		return nil, status.FromContextError(r.ctx.Err()).Err()
	}
	if r.pos >= len(r.data) {
		return nil, io.EOF
	}
	end := min(r.pos+r.chunkSize, len(r.data))
	if r.stall && r.sent+(end-r.pos) > r.stallAfter {
		end = r.pos + (r.stallAfter - r.sent)
	}
	chunk := append([]byte(nil), r.data[r.pos:end]...)
	r.pos = end
	r.sent += len(chunk)
	return &bytestream_proto.ReadResponse{Data: chunk}, nil
}

func (r *stallingReadClient) Header() (metadata.MD, error) { return nil, nil }
func (r *stallingReadClient) Trailer() metadata.MD         { return nil }
func (r *stallingReadClient) CloseSend() error             { return nil }
func (r *stallingReadClient) Context() context.Context     { return r.ctx }
func (r *stallingReadClient) SendMsg(any) error            { return nil }
func (r *stallingReadClient) RecvMsg(any) error            { return nil }

func TestStreamReadResumesAfterIdleTimeout(t *testing.T) {
	blob := testBlob(1000)
	fake := &stallingByteStreamClient{blob: blob, stallAfter: 100}
	c := testCAS(fake, nil)
	policy := testRetryPolicy()
	policy.IdleTimeout = 20 * time.Millisecond
	c.retry = newRetryConfig(policy)

	rc, err := c.streamReadOne(context.Background(), SHA256(make([]byte, 32), int64(len(blob))))
	if err != nil {
		t.Fatalf("streamReadOne: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != len(blob) {
		t.Fatalf("read %d bytes, want %d", len(got), len(blob))
	}
	// The stalled stream was torn down and resumed at the offset reached.
	if want := []int64{0, 100}; !equalInt64s(fake.offsets, want) {
		t.Fatalf("resume offsets = %v, want %v", fake.offsets, want)
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRetryPolicyFromEnvOverrides(t *testing.T) {
	t.Setenv(EnvRetryMaxAttempts, "3")
	t.Setenv(EnvRetryBaseDelay, "10ms")
	t.Setenv(EnvRetryMaxDelay, "20ms")
	t.Setenv(EnvRPCTimeout, "0")
	t.Setenv(EnvIdleTimeout, "5s")

	policy := RetryPolicyFromEnv()
	want := RetryPolicy{MaxAttempts: 3, BaseDelay: 10 * time.Millisecond, MaxDelay: 20 * time.Millisecond, RPCTimeout: 0, IdleTimeout: 5 * time.Second}
	if policy != want {
		t.Fatalf("policy = %+v, want %+v", policy, want)
	}
}

func TestRetryPolicyFromEnvIgnoresGarbage(t *testing.T) {
	t.Setenv(EnvRetryMaxAttempts, "lots")
	t.Setenv(EnvRetryBaseDelay, "-1s")
	policy := RetryPolicyFromEnv()
	if policy != DefaultRetryPolicy() {
		t.Fatalf("policy = %+v, want the defaults", policy)
	}
}

func TestRetriableClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"unavailable", status.Error(codes.Unavailable, ""), true},
		{"internal", status.Error(codes.Internal, ""), true},
		{"aborted", status.Error(codes.Aborted, ""), true},
		{"unknown", status.Error(codes.Unknown, ""), true},
		{"deadline exceeded", status.Error(codes.DeadlineExceeded, ""), true},
		{"resource exhausted", status.Error(codes.ResourceExhausted, ""), true},
		{"cancelled by someone else", status.Error(codes.Canceled, ""), true},
		{"not found", status.Error(codes.NotFound, ""), false},
		{"invalid argument", status.Error(codes.InvalidArgument, ""), false},
		{"permission denied", status.Error(codes.PermissionDenied, ""), false},
		{"wrapped status", casErr(status.Error(codes.Unavailable, "")), true},
		{"local error", errors.New("short read"), false},
		{"eof", io.EOF, false},
	} {
		if got := retriable(context.Background(), tc.err); got != tc.want {
			t.Errorf("retriable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if retriable(cancelled, status.Error(codes.Unavailable, "")) {
		t.Error("retriable() = true after our own context was cancelled, want false")
	}
}
