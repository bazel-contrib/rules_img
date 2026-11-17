package cas

import (
	"context"
	"encoding/hex"
	"io"
	"slices"
	"strings"
	"testing"

	bytestream_proto "google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/blake3sum"
	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

func blake3Digest(content []byte) Digest {
	sum, size, err := blake3sum.Sum(strings.NewReader(string(content)))
	if err != nil {
		panic(err)
	}
	return BLAKE3(sum, size)
}

func TestDigestResourceName(t *testing.T) {
	hash := []byte{0xab, 0xcd}
	for _, tc := range []struct {
		name   string
		digest Digest
		want   string
	}{
		{"sha256 has no function in the name", SHA256(hash, 7), "blobs/abcd/7"},
		{"sha512 has no function in the name", SHA512(hash, 7), "blobs/abcd/7"},
		// BLAKE3 and SHA256 hashes have the same length, so it has to be spelled out.
		{"blake3 is spelled out", BLAKE3(hash, 7), "blobs/blake3/abcd/7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.digest.resourceName(); got != tc.want {
				t.Errorf("resourceName() = %q, want %q", got, tc.want)
			}
		})
	}
}

// resourceRecordingByteStream records the resource name of every read.
type resourceRecordingByteStream struct {
	*fakeByteStreamClient
	resources []string
}

func (r *resourceRecordingByteStream) Read(ctx context.Context, in *bytestream_proto.ReadRequest, opts ...grpc.CallOption) (bytestream_proto.ByteStream_ReadClient, error) {
	r.resources = append(r.resources, in.ResourceName)
	return r.fakeByteStreamClient.Read(ctx, in, opts...)
}

func TestStreamReadUsesTheDigestFunctionInTheResourceName(t *testing.T) {
	blob := testBlob(300)
	for _, tc := range []struct {
		name         string
		digest       Digest
		instanceName string
		want         string
	}{
		{"sha256", SHA256([]byte{1, 2}, 300), "", "blobs/0102/300"},
		{"blake3", BLAKE3([]byte{1, 2}, 300), "", "blobs/blake3/0102/300"},
		{"blake3 with an instance name", BLAKE3([]byte{1, 2}, 300), "main", "main/blobs/blake3/0102/300"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &resourceRecordingByteStream{fakeByteStreamClient: &fakeByteStreamClient{blob: blob, chunkSize: 64}}
			c := testCAS(fake, nil)
			c.capabilities.DigestFunctionBLAKE3 = true
			c.capabilities.MaxBatchTotalSizeBytes = 100 // below the blob, so it is streamed
			c.instanceName = tc.instanceName

			reader, err := c.ReaderForBlob(context.Background(), tc.digest)
			if err != nil {
				t.Fatal(err)
			}
			if got := readAllAndClose(t, reader); len(got) != len(blob) {
				t.Fatalf("read %d bytes, want %d", len(got), len(blob))
			}
			if !slices.Equal(fake.resources, []string{tc.want}) {
				t.Errorf("read resource names %q, want [%q]", fake.resources, tc.want)
			}
		})
	}
}

func TestUploadResourceNameCarriesTheDigestFunction(t *testing.T) {
	u := &byteStreamUpload{owner: &CAS{instanceName: "main"}, digest: BLAKE3([]byte{1, 2}, 5)}
	u.newResourceName()
	if !strings.HasPrefix(u.resourceName, "main/uploads/") || !strings.HasSuffix(u.resourceName, "/blobs/blake3/0102/5") {
		t.Errorf("upload resource name = %q", u.resourceName)
	}
}

// findMissingRecorder answers FindMissingBlobs, reporting every blob but the
// ones in held as missing, and records the requests.
type findMissingRecorder struct {
	remoteexecution_proto.ContentAddressableStorageClient
	held     map[string]bool // hex hash
	requests []*remoteexecution_proto.FindMissingBlobsRequest
}

func (f *findMissingRecorder) FindMissingBlobs(_ context.Context, in *remoteexecution_proto.FindMissingBlobsRequest, _ ...grpc.CallOption) (*remoteexecution_proto.FindMissingBlobsResponse, error) {
	f.requests = append(f.requests, in)
	resp := &remoteexecution_proto.FindMissingBlobsResponse{}
	for _, d := range in.BlobDigests {
		if !f.held[d.Hash] {
			resp.MissingBlobDigests = append(resp.MissingBlobDigests, d)
		}
	}
	return resp, nil
}

func TestFindMissingBlobsSetsTheDigestFunction(t *testing.T) {
	content := []byte("a layer")
	blake3 := blake3Digest(content)
	sha256 := blobDigest(content)
	recorder := &findMissingRecorder{held: map[string]bool{blake3.hexHash(): true}}
	c := testCAS(nil, recorder)
	c.capabilities.DigestFunctionBLAKE3 = true

	missing, err := c.FindMissingBlobs(context.Background(), []Digest{blake3})
	if err != nil || len(missing) != 0 {
		t.Fatalf("FindMissingBlobs(blake3) = %v, %v; want the blob found", missing, err)
	}
	missing, err = c.FindMissingBlobs(context.Background(), []Digest{sha256})
	if err != nil || len(missing) != 1 || missing[0].algorithm != "sha256" {
		t.Fatalf("FindMissingBlobs(sha256) = %v, %v; want the sha256 digest reported missing", missing, err)
	}

	if len(recorder.requests) != 2 {
		t.Fatalf("%d requests, want 2", len(recorder.requests))
	}
	if got := recorder.requests[0].DigestFunction; got != remoteexecution_proto.DigestFunction_BLAKE3 {
		t.Errorf("blake3 request digest_function = %v, want BLAKE3", got)
	}
	if got := recorder.requests[0].BlobDigests[0].Hash; got != hex.EncodeToString(blake3.Hash) {
		t.Errorf("blake3 request hash = %s, want %x", got, blake3.Hash)
	}
	if got := recorder.requests[1].DigestFunction; got != remoteexecution_proto.DigestFunction_SHA256 {
		t.Errorf("sha256 request digest_function = %v, want SHA256", got)
	}
}

func TestFindMissingBlobsRejectsAnUnsupportedBLAKE3(t *testing.T) {
	c := testCAS(nil, &findMissingRecorder{})
	if _, err := c.FindMissingBlobs(context.Background(), []Digest{BLAKE3([]byte{1}, 1)}); err == nil {
		t.Fatal("looking up a BLAKE3 digest in a cache that does not support BLAKE3 succeeded")
	}
}

type capabilitiesConn struct {
	grpc.ClientConnInterface
	functions []remoteexecution_proto.DigestFunction_Value
	err       error
}

func (c *capabilitiesConn) Invoke(_ context.Context, _ string, _ any, reply any, _ ...grpc.CallOption) error {
	if c.err != nil {
		return c.err
	}
	*reply.(*remoteexecution_proto.ServerCapabilities) = remoteexecution_proto.ServerCapabilities{
		CacheCapabilities: &remoteexecution_proto.CacheCapabilities{DigestFunctions: c.functions},
	}
	return nil
}

func TestServerDigestFunctions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		functions []remoteexecution_proto.DigestFunction_Value
		want      []string
	}{
		{"sha256 only", []remoteexecution_proto.DigestFunction_Value{remoteexecution_proto.DigestFunction_SHA256}, []string{"sha256"}},
		{"blake3 only", []remoteexecution_proto.DigestFunction_Value{remoteexecution_proto.DigestFunction_BLAKE3}, []string{"blake3"}},
		{"both, in a stable order", []remoteexecution_proto.DigestFunction_Value{remoteexecution_proto.DigestFunction_BLAKE3, remoteexecution_proto.DigestFunction_SHA256}, []string{"sha256", "blake3"}},
		{"unusable functions are left out", []remoteexecution_proto.DigestFunction_Value{remoteexecution_proto.DigestFunction_MD5, remoteexecution_proto.DigestFunction_BLAKE3}, []string{"blake3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ServerDigestFunctions(context.Background(), &capabilitiesConn{functions: tc.functions}, "")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ServerDigestFunctions = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCachingReaderVerifiesBLAKE3Content(t *testing.T) {
	content := blobContent(3, 512)
	digest := blake3Digest(content)

	t.Run("accepts matching content", func(t *testing.T) {
		source := newFakeBlobs()
		source.serveInstead(digest, content)
		reader, err := newTestCache(t, source).ReaderForBlob(context.Background(), digest)
		if err != nil {
			t.Fatal(err)
		}
		if got := readAllAndClose(t, reader); string(got) != string(content) {
			t.Error("served content differs from the blob")
		}
	})

	t.Run("rejects corrupt content", func(t *testing.T) {
		source := newFakeBlobs()
		source.serveInstead(digest, blobContent(4, 512))
		reader, err := newTestCache(t, source).ReaderForBlob(context.Background(), digest)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		if _, err := io.ReadAll(reader); err == nil {
			t.Fatal("reading a blob whose content does not match its BLAKE3 digest succeeded")
		}
	})
}

// batchReadRecorder serves one blob over BatchReadBlobs and records the request.
type batchReadRecorder struct {
	remoteexecution_proto.ContentAddressableStorageClient
	hash    string
	content []byte
	request *remoteexecution_proto.BatchReadBlobsRequest
}

func (b *batchReadRecorder) BatchReadBlobs(_ context.Context, in *remoteexecution_proto.BatchReadBlobsRequest, _ ...grpc.CallOption) (*remoteexecution_proto.BatchReadBlobsResponse, error) {
	b.request = in
	return &remoteexecution_proto.BatchReadBlobsResponse{Responses: []*remoteexecution_proto.BatchReadBlobsResponse_Response{{
		Digest: &remoteexecution_proto.Digest{Hash: b.hash, SizeBytes: int64(len(b.content))},
		Data:   b.content,
	}}}, nil
}

func TestBatchReadSetsTheDigestFunction(t *testing.T) {
	content := []byte("a small layer")
	digest := blake3Digest(content)
	recorder := &batchReadRecorder{hash: digest.hexHash(), content: content}
	c := testCAS(nil, recorder)
	c.capabilities.DigestFunctionBLAKE3 = true

	data, err := c.ReadBlob(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(content) {
		t.Errorf("read %q, want %q", data, content)
	}
	if got := recorder.request.DigestFunction; got != remoteexecution_proto.DigestFunction_BLAKE3 {
		t.Errorf("BatchReadBlobs digest_function = %v, want BLAKE3", got)
	}
}
