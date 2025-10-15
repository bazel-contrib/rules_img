package cas

import (
	"context"
	"fmt"
	"io"

	"google.golang.org/genproto/googleapis/bytestream"

	"github.com/google/uuid"

	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

func (c *CAS) WriteBlob(ctx context.Context, digest Digest, r io.Reader) error {
	if !c.capabilities.supportedDigestFunction(digest.algorithm) {
		return fmt.Errorf("unsupported digest algorithm: %s", digest.algorithm)
	}
	if digest.SizeBytes == 0 {
		return nil // blob is empty
	}
	if digest.SizeBytes <= c.capabilities.MaxBatchTotalSizeBytes {
		// If the blob is small enough, we can upload it with a single request.
		data, err := io.ReadAll(r)
		if err != nil {
			return fmt.Errorf("reading blob data for %x: %w", digest.Hash, err)
		}
		return c.batchUploadOne(ctx, digest, data)
	}
	// If the blob is too large, we need to upload it in chunks.
	return c.streamUploadOne(ctx, digest, r)
}

func (c *CAS) batchUploadOne(ctx context.Context, digest Digest, data []byte) error {
	resp, err := c.casClient.BatchUpdateBlobs(ctx, &remoteexecution_proto.BatchUpdateBlobsRequest{
		Requests: []*remoteexecution_proto.BatchUpdateBlobsRequest_Request{{
			Digest: digest.protoDigest(),
			Data:   data,
		}},
		DigestFunction: digest.protoDigestFunction(),
	})
	if err != nil {
		return fmt.Errorf("batch uploading blob %x: %w", digest.Hash, casErr(err))
	}
	if len(resp.Responses) != 1 {
		return fmt.Errorf("unexpected number of responses for batch upload: got %d, want 1", len(resp.Responses))
	}
	if resp.Responses[0].Status.Code != 0 {
		return fmt.Errorf("batch upload to remote cache failed for blob %x: %s", digest.Hash, resp.Responses[0].Status.String())
	}
	return nil
}

func (c *CAS) streamUploadOne(ctx context.Context, digest Digest, r io.Reader) error {
	stream, err := c.byteStreamClient.Write(ctx)
	if err != nil {
		return fmt.Errorf("creating bytestream client for writing: %w", casErr(err))
	}

	resourceName := fmt.Sprintf("uploads/%s/blobs/%x/%d", uuid.NewString(), digest.Hash, digest.SizeBytes)
	buf := make([]byte, c.capabilities.MaxBatchTotalSizeBytes)
	var offset int64
	var eof bool
	for !eof {
		n, err := r.Read(buf)
		if err != nil && err != io.EOF {
			_ = stream.CloseSend()
			return fmt.Errorf("reading blob data: %w", err)
		}
		if err == io.EOF {
			eof = true
		}
		last := offset+int64(n) >= digest.SizeBytes
		if err := stream.Send(&bytestream.WriteRequest{
			ResourceName: resourceName,
			WriteOffset:  offset,
			FinishWrite:  eof || last,
			Data:         buf[:n],
		}); err != nil {
			_ = stream.CloseSend()
			if err == io.EOF {
				eof = true
			} else {
				return fmt.Errorf("sending write request: %w", casErr(err))
			}
		}
		offset += int64(n)
		if last {
			eof = true
		}
	}
	if offset != digest.SizeBytes {
		return fmt.Errorf("expected to write %d bytes, but wrote %d bytes", digest.SizeBytes, offset)
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("closing stream: %w", err)
	}
	if resp.CommittedSize != digest.SizeBytes {
		return fmt.Errorf("committed size %d does not match expected size %d for blob %x", resp.CommittedSize, digest.SizeBytes, digest.Hash)
	}
	return nil
}
