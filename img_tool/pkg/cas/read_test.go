package cas

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"testing"
	"time"

	bytestream_proto "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

// fakeByteStreamClient serves a fixed blob over the ByteStream Read RPC and can
// be programmed to tear each connection down after delivering a number of
// bytes, mimicking an HTTP/2 RST_STREAM mid-transfer.
type fakeByteStreamClient struct {
	blob      []byte
	chunkSize int
	// failAfterBytes[i] is how many bytes connection i delivers before returning
	// failErr instead of continuing. Connections past the end of the slice serve
	// to EOF.
	failAfterBytes []int
	failErr        error

	conns   int
	offsets []int64 // recorded ReadOffset per connection, in order
}

func (f *fakeByteStreamClient) Read(ctx context.Context, in *bytestream_proto.ReadRequest, _ ...grpc.CallOption) (bytestream_proto.ByteStream_ReadClient, error) {
	idx := f.conns
	f.conns++
	f.offsets = append(f.offsets, in.ReadOffset)
	failAfter := -1
	if idx < len(f.failAfterBytes) {
		failAfter = f.failAfterBytes[idx]
	}
	return &fakeReadClient{
		ctx:       ctx,
		data:      f.blob[in.ReadOffset:],
		chunkSize: f.chunkSize,
		failAfter: failAfter,
		failErr:   f.failErr,
	}, nil
}

func (f *fakeByteStreamClient) Write(context.Context, ...grpc.CallOption) (bytestream_proto.ByteStream_WriteClient, error) {
	panic("not implemented")
}

func (f *fakeByteStreamClient) QueryWriteStatus(context.Context, *bytestream_proto.QueryWriteStatusRequest, ...grpc.CallOption) (*bytestream_proto.QueryWriteStatusResponse, error) {
	panic("not implemented")
}

type fakeReadClient struct {
	ctx       context.Context
	data      []byte
	chunkSize int
	failAfter int // -1 => never fail
	failErr   error

	pos  int
	sent int
}

func (r *fakeReadClient) Recv() (*bytestream_proto.ReadResponse, error) {
	if r.failAfter >= 0 && r.sent >= r.failAfter {
		return nil, r.failErr
	}
	if r.pos >= len(r.data) {
		return nil, io.EOF
	}
	end := r.pos + r.chunkSize
	if end > len(r.data) {
		end = len(r.data)
	}
	if r.failAfter >= 0 && r.sent+(end-r.pos) > r.failAfter {
		// don't overshoot the programmed failure point within a chunk
		end = r.pos + (r.failAfter - r.sent)
	}
	chunk := append([]byte(nil), r.data[r.pos:end]...)
	r.pos = end
	r.sent += len(chunk)
	return &bytestream_proto.ReadResponse{Data: chunk}, nil
}

func (r *fakeReadClient) Header() (metadata.MD, error) { return nil, nil }
func (r *fakeReadClient) Trailer() metadata.MD         { return nil }
func (r *fakeReadClient) CloseSend() error             { return nil }
func (r *fakeReadClient) Context() context.Context     { return r.ctx }
func (r *fakeReadClient) SendMsg(any) error            { return nil }
func (r *fakeReadClient) RecvMsg(any) error            { return nil }

func testBlob(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// testRetryPolicy is the default policy with the waits shrunk so tests don't
// sleep, and without the idle watchdog (fakes answer instantly).
func testRetryPolicy() RetryPolicy {
	policy := DefaultRetryPolicy()
	policy.BaseDelay = time.Millisecond
	policy.MaxDelay = time.Millisecond
	policy.IdleTimeout = 0
	return policy
}

// maxRetries is how many retries testRetryPolicy allows after the first attempt.
func maxRetries() int { return testRetryPolicy().MaxAttempts - 1 }

// testCAS is a CAS client wired to fakes, with a fast retry policy.
func testCAS(byteStreamClient bytestream_proto.ByteStreamClient, casClient remoteexecution_proto.ContentAddressableStorageClient) *CAS {
	return &CAS{
		byteStreamClient: byteStreamClient,
		casClient:        casClient,
		capabilities: capabilities{
			DigestFunctionSHA256:   true,
			MaxBatchTotalSizeBytes: 2 * 1024 * 1024,
		},
		retry: newRetryConfig(testRetryPolicy()),
	}
}

// rstErr mimics the error surfaced when the server sends RST_STREAM NO_ERROR.
func rstErr() error {
	return status.Error(codes.Internal, "stream terminated by RST_STREAM with error code: NO_ERROR")
}

func TestStreamReadReconnectResumesAfterRST(t *testing.T) {
	blob := testBlob(1000)
	fake := &fakeByteStreamClient{
		blob:      blob,
		chunkSize: 64,
		// conn 0 delivers 100 bytes then RSTs; conn 1 delivers another 250 then
		// RSTs; conn 2 serves the rest to EOF.
		failAfterBytes: []int{100, 250},
		failErr:        rstErr(),
	}
	c := testCAS(fake, nil)

	rc, err := c.streamReadOne(context.Background(), SHA256(make([]byte, 32), int64(len(blob))))
	if err != nil {
		t.Fatalf("streamReadOne: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("blob mismatch: got %d bytes, want %d", len(got), len(blob))
	}
	// The second and third connections must resume exactly where the previous
	// one stopped, so no bytes are duplicated or skipped.
	wantOffsets := []int64{0, 100, 350}
	if !reflect.DeepEqual(fake.offsets, wantOffsets) {
		t.Fatalf("resume offsets = %v, want %v", fake.offsets, wantOffsets)
	}
}

func TestStreamReadGivesUpAfterMaxReconnects(t *testing.T) {
	blob := testBlob(1000)
	// Every connection RSTs immediately without delivering any bytes, so the
	// consecutive-failure counter is never reset.
	failPlan := make([]int, maxRetries()+2)
	fake := &fakeByteStreamClient{
		blob:           blob,
		chunkSize:      64,
		failAfterBytes: failPlan, // all zeros => fail before any data
		failErr:        rstErr(),
	}
	c := testCAS(fake, nil)

	rc, err := c.streamReadOne(context.Background(), SHA256(make([]byte, 32), int64(len(blob))))
	if err != nil {
		t.Fatalf("streamReadOne: %v", err)
	}
	defer rc.Close()

	_, err = io.ReadAll(rc)
	if err == nil {
		t.Fatal("expected an error after exhausting reconnects, got nil")
	}
	// initial connection + maxRetries() retries
	wantConns := 1 + maxRetries()
	if fake.conns != wantConns {
		t.Fatalf("connections = %d, want %d", fake.conns, wantConns)
	}
}

func TestStreamReadDoesNotRetryNonTransient(t *testing.T) {
	blob := testBlob(1000)
	fake := &fakeByteStreamClient{
		blob:           blob,
		chunkSize:      64,
		failAfterBytes: []int{100},
		failErr:        status.Error(codes.NotFound, "blob not found"),
	}
	c := testCAS(fake, nil)

	rc, err := c.streamReadOne(context.Background(), SHA256(make([]byte, 32), int64(len(blob))))
	if err != nil {
		t.Fatalf("streamReadOne: %v", err)
	}
	defer rc.Close()

	_, err = io.ReadAll(rc)
	if err == nil {
		t.Fatal("expected NotFound error to propagate, got nil")
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("error code = %s, want %s", status.Code(err), codes.NotFound)
	}
	// Only the initial connection should have been made; NotFound is not retried.
	if fake.conns != 1 {
		t.Fatalf("connections = %d, want 1 (no retry on non-transient error)", fake.conns)
	}
}

func TestStreamReadDoesNotRetryOnCallerCancel(t *testing.T) {
	blob := testBlob(1000)
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeByteStreamClient{
		blob:      blob,
		chunkSize: 64,
		// Deliver 100 bytes, then fail. The test cancels the caller context
		// before the failure is observed.
		failAfterBytes: []int{100},
		failErr:        rstErr(),
	}
	c := testCAS(fake, nil)

	rc, err := c.streamReadOne(ctx, SHA256(make([]byte, 32), int64(len(blob))))
	if err != nil {
		t.Fatalf("streamReadOne: %v", err)
	}
	defer rc.Close()

	// Drain the first 100 bytes that arrive before the RST.
	buf := make([]byte, 100)
	if _, err := io.ReadFull(rc, buf); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	// Now cancel: the subsequent RST must not trigger a reconnect.
	cancel()
	if _, err := io.ReadAll(rc); err == nil {
		t.Fatal("expected error after caller cancellation, got nil")
	}
	if fake.conns != 1 {
		t.Fatalf("connections = %d, want 1 (no reconnect after caller cancel)", fake.conns)
	}
}
