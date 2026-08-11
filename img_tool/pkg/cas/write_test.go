package cas

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	bytestream_proto "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeWriteServer is a ByteStream write endpoint that accumulates the blob under
// its resource name and can be programmed to fail part way through.
type fakeWriteServer struct {
	// commitPlan[i] is how many bytes stream i commits before it stops
	// committing. -1 commits everything, as do streams past the end of the plan.
	commitPlan []int
	// hardStop makes a stream fail the Send that crosses its limit, as a server
	// that tears the stream down does. Otherwise it keeps accepting and discarding
	// -- what a client that had already buffered data into the stream sees -- and
	// fails at CloseAndRecv.
	hardStop bool
	// failOnClose makes a stream that accepted everything still fail its
	// CloseAndRecv, as a lost acknowledgement would.
	failOnClose bool
	failErr     error
	// queryCommitted, when >= 0, is what QueryWriteStatus reports instead of what
	// the server actually holds — a server that lost buffered data.
	queryCommitted int64
	// queryErr, when set, is returned by QueryWriteStatus.
	queryErr error

	mu        sync.Mutex
	data      map[string][]byte // committed bytes per resource name
	streams   int
	names     []string // resource name of each stream, in order
	offsets   []int64  // first write_offset of each stream, in order
	queries   int
	completed map[string]bool
}

func newFakeWriteServer() *fakeWriteServer {
	return &fakeWriteServer{
		failErr:        unavailable(),
		queryCommitted: -1,
		data:           make(map[string][]byte),
		completed:      make(map[string]bool),
	}
}

func (f *fakeWriteServer) Read(context.Context, *bytestream_proto.ReadRequest, ...grpc.CallOption) (bytestream_proto.ByteStream_ReadClient, error) {
	panic("not implemented")
}

func (f *fakeWriteServer) Write(ctx context.Context, _ ...grpc.CallOption) (bytestream_proto.ByteStream_WriteClient, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	commit := -1
	if f.streams < len(f.commitPlan) {
		commit = f.commitPlan[f.streams]
	}
	f.streams++
	return &fakeWriteStream{ctx: ctx, server: f, commit: commit}, nil
}

func (f *fakeWriteServer) QueryWriteStatus(_ context.Context, in *bytestream_proto.QueryWriteStatusRequest, _ ...grpc.CallOption) (*bytestream_proto.QueryWriteStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	committed := int64(len(f.data[in.ResourceName]))
	if f.queryCommitted >= 0 {
		committed = f.queryCommitted
	}
	return &bytestream_proto.QueryWriteStatusResponse{
		CommittedSize: committed,
		Complete:      f.completed[in.ResourceName],
	}, nil
}

// blob returns the bytes committed under the only resource name the server saw
// data for, and fails the test if there is more than one.
func (f *fakeWriteServer) blob(t *testing.T) []byte {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var found []byte
	for _, data := range f.data {
		if len(data) == 0 {
			continue
		}
		if found != nil {
			t.Fatalf("server holds data under more than one resource name")
		}
		found = data
	}
	return found
}

// blobUnder returns the bytes committed under one resource name.
func (f *fakeWriteServer) blobUnder(name string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[name]
}

type fakeWriteStream struct {
	ctx    context.Context
	server *fakeWriteServer
	commit int // -1: unlimited

	committed int
	dropping  bool // past the commit limit: accepted and thrown away
	failed    bool
	finished  bool
	name      string
}

func (s *fakeWriteStream) Send(req *bytestream_proto.WriteRequest) error {
	if s.failed {
		// grpc-go reports a stream the server tore down as io.EOF on Send; the
		// status is on CloseAndRecv.
		return io.EOF
	}
	s.server.mu.Lock()
	defer s.server.mu.Unlock()
	if s.name == "" {
		s.name = req.ResourceName
		s.server.names = append(s.server.names, req.ResourceName)
		s.server.offsets = append(s.server.offsets, req.WriteOffset)
	}
	held := int64(len(s.server.data[s.name]))
	if req.WriteOffset != held {
		return status.Errorf(codes.InvalidArgument, "write_offset %d does not match committed_size %d", req.WriteOffset, held)
	}
	data := req.Data
	if s.commit >= 0 && s.committed+len(data) > s.commit {
		// Commit the prefix that fits and lose the rest.
		data = data[:s.commit-s.committed]
		s.dropping = true
		s.failed = s.server.hardStop
	}
	s.server.data[s.name] = append(s.server.data[s.name], data...)
	s.committed += len(data)
	if req.FinishWrite && !s.dropping {
		s.finished = true
		s.server.completed[s.name] = true
	}
	if s.failed {
		return io.EOF
	}
	return nil
}

func (s *fakeWriteStream) CloseAndRecv() (*bytestream_proto.WriteResponse, error) {
	s.server.mu.Lock()
	defer s.server.mu.Unlock()
	if s.dropping || (s.finished && s.server.failOnClose) {
		return nil, s.server.failErr
	}
	if !s.finished {
		return nil, status.Error(codes.Internal, "stream closed before the write finished")
	}
	return &bytestream_proto.WriteResponse{CommittedSize: int64(len(s.server.data[s.name]))}, nil
}

func (s *fakeWriteStream) Header() (metadata.MD, error) { return nil, nil }
func (s *fakeWriteStream) Trailer() metadata.MD         { return nil }
func (s *fakeWriteStream) CloseSend() error             { return nil }
func (s *fakeWriteStream) Context() context.Context     { return s.ctx }
func (s *fakeWriteStream) SendMsg(any) error            { return nil }
func (s *fakeWriteStream) RecvMsg(any) error            { return nil }

// uploadCAS is a client that streams anything larger than chunkSize.
func uploadCAS(byteStreamClient bytestream_proto.ByteStreamClient, chunkSize int64) *CAS {
	c := testCAS(byteStreamClient, nil)
	c.capabilities.MaxBatchTotalSizeBytes = chunkSize
	return c
}

func digestOf(blob []byte) Digest {
	return SHA256(make([]byte, 32), int64(len(blob)))
}

// unseekable hides the Seek method of the underlying reader, standing in for a
// source that cannot be rewound (an HTTP request body, say).
type unseekable struct{ r io.Reader }

func (u unseekable) Read(p []byte) (int, error) { return u.r.Read(p) }

func TestStreamUploadWritesWholeBlob(t *testing.T) {
	blob := testBlob(300)
	server := newFakeWriteServer()
	c := uploadCAS(server, 100)

	if err := c.WriteBlob(context.Background(), digestOf(blob), bytes.NewReader(blob)); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if got := server.blob(t); !bytes.Equal(got, blob) {
		t.Fatalf("uploaded %d bytes, want %d", len(got), len(blob))
	}
}

func TestStreamUploadResumesFromBufferedChunk(t *testing.T) {
	blob := testBlob(300)
	server := newFakeWriteServer()
	// The server tears the stream down half way through the second chunk, which
	// we still hold -- so even a source that cannot be rewound resumes.
	server.commitPlan = []int{150}
	server.hardStop = true
	c := uploadCAS(server, 100)

	if err := c.WriteBlob(context.Background(), digestOf(blob), unseekable{bytes.NewReader(blob)}); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if got := server.blob(t); !bytes.Equal(got, blob) {
		t.Fatalf("uploaded blob does not match the source (%d bytes)", len(got))
	}
	// The second stream picks up exactly where the server got to, and under the
	// same upload id, so nothing is sent twice.
	if want := []int64{0, 150}; !equalInt64s(server.offsets, want) {
		t.Fatalf("stream offsets = %v, want %v", server.offsets, want)
	}
	if server.names[0] != server.names[1] {
		t.Fatalf("resume used a new upload id (%q -> %q), want the same one", server.names[0], server.names[1])
	}
	if server.queries != 1 {
		t.Fatalf("QueryWriteStatus calls = %d, want 1", server.queries)
	}
}

func TestStreamUploadTreatsCommittedBlobAsDone(t *testing.T) {
	blob := testBlob(300)
	server := newFakeWriteServer()
	// Everything arrives, but the acknowledgement is lost. QueryWriteStatus then
	// reports the blob as complete, which is not a failure.
	server.failOnClose = true
	c := uploadCAS(server, 100)

	if err := c.WriteBlob(context.Background(), digestOf(blob), bytes.NewReader(blob)); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if server.streams != 1 {
		t.Fatalf("write streams = %d, want 1 (nothing left to send)", server.streams)
	}
}

func TestStreamUploadRewindsSeekableSource(t *testing.T) {
	blob := testBlob(300)
	server := newFakeWriteServer()
	// The server commits only the first chunk but keeps taking data, so by the
	// time the failure surfaces we have read past what it kept. Only rewinding the
	// source can recover.
	server.commitPlan = []int{100}
	c := uploadCAS(server, 100)

	if err := c.WriteBlob(context.Background(), digestOf(blob), bytes.NewReader(blob)); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if got := server.blob(t); !bytes.Equal(got, blob) {
		t.Fatalf("uploaded blob does not match the source (%d bytes)", len(got))
	}
	if want := []int64{0, 100}; !equalInt64s(server.offsets, want) {
		t.Fatalf("stream offsets = %v, want %v", server.offsets, want)
	}
}

func TestStreamUploadReportsUnresumableFailure(t *testing.T) {
	blob := testBlob(300)
	server := newFakeWriteServer()
	// Same as above, but the source cannot be rewound: the bytes the server threw
	// away are gone, and the caller has to be told.
	server.commitPlan = []int{100}
	c := uploadCAS(server, 100)

	err := c.WriteBlob(context.Background(), digestOf(blob), unseekable{bytes.NewReader(blob)})
	if err == nil {
		t.Fatal("expected an error when the upload cannot be resumed")
	}
	if !strings.Contains(err.Error(), "cannot be rewound") {
		t.Fatalf("error = %v, want it to say the source cannot be rewound", err)
	}
	if server.streams != 1 {
		t.Fatalf("write streams = %d, want 1 (no pointless second attempt)", server.streams)
	}
}

func TestStreamUploadRestartsWhenQueryWriteStatusIsUnimplemented(t *testing.T) {
	blob := testBlob(300)
	server := newFakeWriteServer()
	// The first stream dies inside the first chunk, which is still buffered, so
	// the upload can start over even without QueryWriteStatus.
	server.commitPlan = []int{50}
	server.hardStop = true
	server.queryErr = status.Error(codes.Unimplemented, "not implemented")
	c := uploadCAS(server, 100)

	if err := c.WriteBlob(context.Background(), digestOf(blob), bytes.NewReader(blob)); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	// Restarting uses a fresh upload id, so the server never sees offset 0 written
	// twice under one resource name.
	if server.names[0] == server.names[1] {
		t.Fatalf("restart reused upload id %q, want a fresh one", server.names[0])
	}
	if want := []int64{0, 0}; !equalInt64s(server.offsets, want) {
		t.Fatalf("stream offsets = %v, want %v", server.offsets, want)
	}
	if got := server.blobUnder(server.names[1]); !bytes.Equal(got, blob) {
		t.Fatalf("uploaded blob does not match the source (%d bytes)", len(got))
	}
}

func TestBatchUploadRetriesTransientFailure(t *testing.T) {
	blob := testBlob(100)
	fake := &fakeCASClient{failErr: unavailable(), failTimes: 2}
	c := testCAS(nil, fake)

	if err := c.WriteBlob(context.Background(), digestOf(blob), bytes.NewReader(blob)); err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if fake.callCount() != 3 {
		t.Fatalf("BatchUpdateBlobs calls = %d, want 3", fake.callCount())
	}
}

func TestBatchUploadDoesNotRetryPermanentFailure(t *testing.T) {
	blob := testBlob(100)
	fake := &fakeCASClient{failErr: status.Error(codes.InvalidArgument, "digest mismatch"), failTimes: 99}
	c := testCAS(nil, fake)

	err := c.WriteBlob(context.Background(), digestOf(blob), bytes.NewReader(blob))
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("error = %v, want InvalidArgument", err)
	}
	if fake.callCount() != 1 {
		t.Fatalf("BatchUpdateBlobs calls = %d, want 1", fake.callCount())
	}
}
