package cas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

// batchCASClient answers BatchReadBlobs from memory and records the digest
// lists it was asked for, so a test can see how a read was split into requests.
type batchCASClient struct {
	blobs map[string][]byte // hex hash -> content

	// mu guards requests: prefetching puts several batches in flight at once.
	mu       sync.Mutex
	requests [][]string // hex hashes per BatchReadBlobs call, in arrival order
	// reorder returns responses in reverse request order, which the spec allows.
	reorder bool
	// statusFor overrides the per-blob status of a hash.
	statusFor map[string]error
	// dataFor overrides the bytes returned for a hash.
	dataFor map[string][]byte
	// omit drops a hash from the response entirely.
	omit map[string]bool
	// compressor is reported on every response entry.
	compressor remoteexecution_proto.Compressor_Value
}

func newBatchCASClient(blobs ...[]byte) *batchCASClient {
	c := &batchCASClient{blobs: make(map[string][]byte)}
	for _, blob := range blobs {
		sum := sha256.Sum256(blob)
		c.blobs[hex.EncodeToString(sum[:])] = blob
	}
	return c
}

func (c *batchCASClient) digest(blob []byte) Digest {
	sum := sha256.Sum256(blob)
	return SHA256(sum[:], int64(len(blob)))
}

func (c *batchCASClient) BatchReadBlobs(_ context.Context, in *remoteexecution_proto.BatchReadBlobsRequest, _ ...grpc.CallOption) (*remoteexecution_proto.BatchReadBlobsResponse, error) {
	asked := make([]string, 0, len(in.Digests))
	for _, d := range in.Digests {
		asked = append(asked, d.Hash)
	}
	c.mu.Lock()
	c.requests = append(c.requests, asked)
	c.mu.Unlock()

	var responses []*remoteexecution_proto.BatchReadBlobsResponse_Response
	for _, d := range in.Digests {
		if c.omit[d.Hash] {
			continue
		}
		response := &remoteexecution_proto.BatchReadBlobsResponse_Response{Digest: d, Compressor: c.compressor}
		if err, ok := c.statusFor[d.Hash]; ok {
			response.Status = status.Convert(err).Proto()
		} else if data, ok := c.dataFor[d.Hash]; ok {
			response.Data = data
		} else {
			response.Data = c.blobs[d.Hash]
		}
		responses = append(responses, response)
	}
	if c.reorder {
		slices.Reverse(responses)
	}
	return &remoteexecution_proto.BatchReadBlobsResponse{Responses: responses}, nil
}

func (c *batchCASClient) FindMissingBlobs(context.Context, *remoteexecution_proto.FindMissingBlobsRequest, ...grpc.CallOption) (*remoteexecution_proto.FindMissingBlobsResponse, error) {
	panic("not implemented")
}

func (c *batchCASClient) BatchUpdateBlobs(context.Context, *remoteexecution_proto.BatchUpdateBlobsRequest, ...grpc.CallOption) (*remoteexecution_proto.BatchUpdateBlobsResponse, error) {
	panic("not implemented")
}

func (c *batchCASClient) GetTree(context.Context, *remoteexecution_proto.GetTreeRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[remoteexecution_proto.GetTreeResponse], error) {
	panic("not implemented")
}

func (c *batchCASClient) SplitBlob(context.Context, *remoteexecution_proto.SplitBlobRequest, ...grpc.CallOption) (*remoteexecution_proto.SplitBlobResponse, error) {
	panic("not implemented")
}

func (c *batchCASClient) SpliceBlob(context.Context, *remoteexecution_proto.SpliceBlobRequest, ...grpc.CallOption) (*remoteexecution_proto.SpliceBlobResponse, error) {
	panic("not implemented")
}

// smallBlobs returns n distinct blobs of the given size.
func smallBlobs(n, size int) [][]byte {
	blobs := make([][]byte, n)
	for i := range blobs {
		blob := bytes.Repeat([]byte{byte(i), byte(i >> 8)}, size/2)
		blobs[i] = blob
	}
	return blobs
}

func digestsOf(c *batchCASClient, blobs [][]byte) []Digest {
	digests := make([]Digest, len(blobs))
	for i, blob := range blobs {
		digests[i] = c.digest(blob)
	}
	return digests
}

// requestSizes returns how many digests each BatchReadBlobs call asked for,
// largest first. Prefetching runs several batches at once, so the arrival order
// says nothing; the sizes still do.
func (c *batchCASClient) requestSizes() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	sizes := make([]int, len(c.requests))
	for i, request := range c.requests {
		sizes[i] = len(request)
	}
	slices.SortFunc(sizes, func(a, b int) int { return b - a })
	return sizes
}

// requestCount is how many BatchReadBlobs calls were made.
func (c *batchCASClient) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// readBlobs reads the whole concatenated stream for digests.
func readBlobs(t *testing.T, c *CAS, digests []Digest) []byte {
	t.Helper()
	rc, err := c.ReaderForBlobs(context.Background(), digests)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The point of the whole exercise: many small blobs come back in a handful of
// requests instead of one per blob.
func TestReaderForBlobsBatchesSmallBlobs(t *testing.T) {
	blobs := smallBlobs(300, 1000)
	client := newBatchCASClient(blobs...)
	c := testCAS(nil, client)
	// 1000-byte blobs at 1128 apiece once framing is charged, 100 KiB per
	// batch: 90 blobs per request, so 300 of them take 4.
	c.capabilities.MaxBatchTotalSizeBytes = 100 * 1024

	got := readBlobs(t, c, digestsOf(client, blobs))

	if want := bytes.Join(blobs, nil); !bytes.Equal(got, want) {
		t.Fatalf("stream is %d bytes, want %d", len(got), len(want))
	}
	if client.requestCount() != 4 {
		t.Fatalf("made %d BatchReadBlobs requests, want 4", client.requestCount())
	}
	for i, request := range client.requests {
		var framed int64
		for _, hash := range request {
			framed += int64(len(client.blobs[hash])) + batchReadEntryOverhead
		}
		if framed > c.capabilities.MaxBatchTotalSizeBytes {
			t.Errorf("request %d costs %d bytes framed, over the %d byte budget", i, framed, c.capabilities.MaxBatchTotalSizeBytes)
		}
	}
}

// Framing on a batch scales with the number of blobs, so each blob is charged
// its own share rather than relying on the fixed headroom batchPayloadBudget
// holds back. A budget that fits ten blobs on payload alone fits fewer.
func TestReaderForBlobsReservesPerBlobFraming(t *testing.T) {
	const size = 1000
	blobs := smallBlobs(10, size)
	client := newBatchCASClient(blobs...)
	c := testCAS(nil, client)
	// Exactly ten blobs' worth of payload, and room for four blobs' framing.
	c.capabilities.MaxBatchTotalSizeBytes = 10*size + 4*batchReadEntryOverhead

	readBlobs(t, c, digestsOf(client, blobs))

	sizes := client.requestSizes()
	if len(sizes) < 2 {
		t.Fatalf("made %d requests; framing was not charged against the budget", len(sizes))
	}
	want := int(c.capabilities.MaxBatchTotalSizeBytes / (size + batchReadEntryOverhead))
	if sizes[0] != want {
		t.Fatalf("largest request held %d blobs, want %d", sizes[0], want)
	}
}

// A blob that fits the budget on payload alone but not once its framing is
// charged is streamed rather than batched. Getting this wrong would leave
// batchEnd with an empty run.
func TestReaderForBlobsStreamsABlobThatOnlyFitsWithoutFraming(t *testing.T) {
	const limit = 4096
	blob := testBlob(limit) // exactly the budget: over it once framed
	client := newBatchCASClient(blob)
	byteStream := &fakeByteStreamClient{blob: blob, chunkSize: 1024}
	c := testCAS(byteStream, client)
	c.capabilities.MaxBatchTotalSizeBytes = limit

	got := readBlobs(t, c, []Digest{client.digest(blob)})

	if !bytes.Equal(got, blob) {
		t.Fatal("stream does not match the blob")
	}
	if client.requestCount() != 0 {
		t.Fatalf("made %d BatchReadBlobs requests for a blob that does not fit framed", client.requestCount())
	}
	if byteStream.conns != 1 {
		t.Fatalf("opened %d ByteStream reads, want 1", byteStream.conns)
	}
}

func TestReaderForBlobsCapsTheDigestCount(t *testing.T) {
	blobs := smallBlobs(maxBatchReadDigests+10, 2)
	client := newBatchCASClient(blobs...)
	c := testCAS(nil, client)

	readBlobs(t, c, digestsOf(client, blobs))

	sizes := client.requestSizes()
	if len(sizes) != 2 {
		t.Fatalf("made %d BatchReadBlobs requests, want 2", len(sizes))
	}
	if sizes[0] != maxBatchReadDigests {
		t.Fatalf("largest request asked for %d digests, want %d", sizes[0], maxBatchReadDigests)
	}
}

// A blob too big for a batch is streamed on its own, and the blobs around it
// still batch.
func TestReaderForBlobsStreamsLargeBlobs(t *testing.T) {
	small := smallBlobs(4, 100)
	large := testBlob(4096)
	client := newBatchCASClient(append(slices.Clone(small), large)...)
	byteStream := &fakeByteStreamClient{blob: large, chunkSize: 512}
	c := testCAS(byteStream, client)
	c.capabilities.MaxBatchTotalSizeBytes = 1024

	order := [][]byte{small[0], small[1], large, small[2], small[3]}
	got := readBlobs(t, c, digestsOf(client, order))

	if want := bytes.Join(order, nil); !bytes.Equal(got, want) {
		t.Fatal("stream does not match the requested blobs in order")
	}
	if client.requestCount() != 2 {
		t.Fatalf("made %d BatchReadBlobs requests, want 2 (one per run of small blobs)", client.requestCount())
	}
	if byteStream.conns != 1 {
		t.Fatalf("opened %d ByteStream reads, want 1", byteStream.conns)
	}
}

// The spec does not promise a response order, so responses are matched by
// digest.
func TestReaderForBlobsMatchesResponsesByDigest(t *testing.T) {
	blobs := smallBlobs(8, 100)
	client := newBatchCASClient(blobs...)
	client.reorder = true
	c := testCAS(nil, client)

	got := readBlobs(t, c, digestsOf(client, blobs))

	if want := bytes.Join(blobs, nil); !bytes.Equal(got, want) {
		t.Fatal("a reordered response reordered the stream")
	}
}

// A blob referenced twice is requested once and delivered twice.
func TestReaderForBlobsDeduplicatesWithinARequest(t *testing.T) {
	blobs := smallBlobs(2, 100)
	client := newBatchCASClient(blobs...)
	c := testCAS(nil, client)

	order := [][]byte{blobs[0], blobs[1], blobs[0]}
	got := readBlobs(t, c, digestsOf(client, order))

	if want := bytes.Join(order, nil); !bytes.Equal(got, want) {
		t.Fatal("a repeated blob was not delivered twice")
	}
	if client.requestCount() != 1 {
		t.Fatalf("made %d requests, want 1", client.requestCount())
	}
	if got := len(client.requests[0]); got != 2 {
		t.Fatalf("asked for %d digests, want 2 (the repeat is asked for once)", got)
	}
}

// An empty blob contributes nothing and costs no request.
func TestReaderForBlobsSkipsEmptyBlobs(t *testing.T) {
	blobs := smallBlobs(2, 100)
	client := newBatchCASClient(blobs...)
	c := testCAS(nil, client)

	digests := []Digest{client.digest(blobs[0]), SHA256(sha256Of(nil), 0), client.digest(blobs[1])}
	got := readBlobs(t, c, digests)

	if want := append(slices.Clone(blobs[0]), blobs[1]...); !bytes.Equal(got, want) {
		t.Fatal("an empty blob changed the stream")
	}
	if client.requestCount() != 1 {
		t.Fatalf("made %d requests, want 1 (an empty blob must not split the batch)", client.requestCount())
	}
}

func TestReaderForBlobsEmptyList(t *testing.T) {
	client := newBatchCASClient()
	c := testCAS(nil, client)

	if got := readBlobs(t, c, nil); len(got) != 0 {
		t.Fatalf("stream = %q, want empty", got)
	}
	if client.requestCount() != 0 {
		t.Fatalf("made %d requests for an empty list", client.requestCount())
	}
}

func TestReaderForBlobsRejectsAMissingResponseEntry(t *testing.T) {
	blobs := smallBlobs(3, 100)
	client := newBatchCASClient(blobs...)
	missing := client.digest(blobs[1])
	client.omit = map[string]bool{missing.hexHash(): true}
	c := testCAS(nil, client)

	rc, err := c.ReaderForBlobs(context.Background(), digestsOf(client, blobs))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "missing from the BatchReadBlobs response") {
		t.Fatalf("error = %v, want it to report the missing entry", err)
	}
}

func TestReaderForBlobsRejectsAWrongSizedBlob(t *testing.T) {
	blobs := smallBlobs(2, 100)
	client := newBatchCASClient(blobs...)
	short := client.digest(blobs[0])
	client.dataFor = map[string][]byte{short.hexHash(): blobs[0][:10]}
	c := testCAS(nil, client)

	rc, err := c.ReaderForBlobs(context.Background(), digestsOf(client, blobs))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "expected 100") {
		t.Fatalf("error = %v, want it to report the size mismatch", err)
	}
}

func TestReaderForBlobsRejectsCompressedData(t *testing.T) {
	blobs := smallBlobs(1, 100)
	client := newBatchCASClient(blobs...)
	client.compressor = remoteexecution_proto.Compressor_ZSTD
	c := testCAS(nil, client)

	rc, err := c.ReaderForBlobs(context.Background(), digestsOf(client, blobs))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "compressed data") {
		t.Fatalf("error = %v, want it to reject the compressed response", err)
	}
}

// A cache miss is reported per blob, and is not retried.
func TestReaderForBlobsReportsAPerBlobStatus(t *testing.T) {
	blobs := smallBlobs(2, 100)
	client := newBatchCASClient(blobs...)
	client.statusFor = map[string]error{
		client.digest(blobs[1]).hexHash(): status.Error(codes.NotFound, "no such blob"),
	}
	c := testCAS(nil, client)

	rc, err := c.ReaderForBlobs(context.Background(), digestsOf(client, blobs))
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	_, err = io.ReadAll(rc)
	if status.Code(errors.Unwrap(err)) != codes.NotFound && !strings.Contains(err.Error(), "no such blob") {
		t.Fatalf("error = %v, want the NotFound status", err)
	}
	if client.requestCount() != 1 {
		t.Fatalf("made %d requests, want 1 (a cache miss is not retried)", client.requestCount())
	}
}

func TestReaderForBlobsRejectsMixedAlgorithms(t *testing.T) {
	c := testCAS(nil, newBatchCASClient())
	c.capabilities.DigestFunctionSHA512 = true

	digests := []Digest{SHA256(sha256Of([]byte("a")), 1), SHA512(sha256Of([]byte("b")), 1)}
	if _, err := c.ReaderForBlobs(context.Background(), digests); err == nil {
		t.Fatal("mixing digest algorithms in one stream was accepted")
	}
}

// Closing partway through must not leave the ByteStream read of a large blob
// open.
func TestReaderForBlobsCloseReleasesTheStream(t *testing.T) {
	large := testBlob(4096)
	client := newBatchCASClient(large)
	byteStream := &fakeByteStreamClient{blob: large, chunkSize: 128}
	c := testCAS(byteStream, client)
	c.capabilities.MaxBatchTotalSizeBytes = 1024

	rc, err := c.ReaderForBlobs(context.Background(), []Digest{client.digest(large)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rc, make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := rc.Read(make([]byte, 1)); err != io.EOF || got != 0 {
		t.Fatalf("read after Close = (%d, %v), want (0, EOF)", got, err)
	}
}

// ReadBlob still works: the single-blob path now goes through the same batch
// call with a one-digest request.
func TestReadBlobStillUsesASingleDigestRequest(t *testing.T) {
	blob := testBlob(100)
	client := newBatchCASClient(blob)
	c := testCAS(nil, client)

	got, err := c.ReadBlob(context.Background(), client.digest(blob))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatal("ReadBlob returned the wrong content")
	}
	if sizes := client.requestSizes(); len(sizes) != 1 || sizes[0] != 1 {
		t.Fatalf("requests = %v, want one request for one digest", client.requests)
	}
}

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
