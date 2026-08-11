package cas

import (
	"context"
	"errors"
	"fmt"
	"io"

	"google.golang.org/genproto/googleapis/bytestream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/google/uuid"

	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

// WriteBlob uploads a blob to the remote cache.
//
// Small blobs go in a single BatchUpdateBlobs call, which is retried outright:
// the request is content-addressed and held in memory, so another attempt sends
// exactly the same bytes.
//
// Larger blobs are streamed, and a stream that fails is resumed the way
// ByteStream prescribes: ask QueryWriteStatus how much the server committed and
// continue from there. How far that gets depends on what we can reproduce. A
// source that seeks (a file, a bytes.Reader) can always be rewound, so those
// uploads resume from any offset. For a plain io.Reader we only still hold the
// chunk currently in flight, which covers a stream the server tore down while we
// were sending it; a failure that leaves the server behind older, already
// discarded data is reported to the caller, who still has the source and can
// start over.
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
	request := &remoteexecution_proto.BatchUpdateBlobsRequest{
		InstanceName: c.instanceName,
		Requests: []*remoteexecution_proto.BatchUpdateBlobsRequest_Request{{
			Digest: digest.protoDigest(),
			Data:   data,
		}},
		DigestFunction: digest.protoDigestFunction(),
	}
	r := c.retry.start(fmt.Sprintf("uploading blob %s (%d bytes) to the remote cache", digest.hexHash(), digest.SizeBytes))
	for {
		err := c.peer(r.attempt).batchUploadOnce(ctx, request)
		if err == nil {
			return nil
		}
		if giveUp := r.next(ctx, err); giveUp != nil {
			return fmt.Errorf("batch uploading blob %x: %w", digest.Hash, casErr(giveUp))
		}
	}
}

// batchUploadOnce is one BatchUpdateBlobs call for a single blob. A transient
// per-blob status is returned as a status error, so the retry loop treats it like
// a transient call-level failure.
func (c *CAS) batchUploadOnce(ctx context.Context, request *remoteexecution_proto.BatchUpdateBlobsRequest) error {
	callCtx, cancel := c.callContext(ctx)
	defer cancel()
	resp, err := c.casClient.BatchUpdateBlobs(callCtx, request)
	if err != nil {
		return err
	}
	if len(resp.Responses) != 1 {
		return fmt.Errorf("unexpected number of responses for batch upload: got %d, want 1", len(resp.Responses))
	}
	if st := resp.Responses[0].Status; st != nil && st.Code != 0 {
		return status.ErrorProto(st)
	}
	return nil
}

func (c *CAS) streamUploadOne(ctx context.Context, digest Digest, r io.Reader) error {
	upload := &byteStreamUpload{
		owner:  c,
		digest: digest,
		src:    r,
		buf:    make([]byte, c.capabilities.MaxBatchTotalSizeBytes),
	}
	// A source we can seek can be rewound to any offset the server asks us to
	// resume from. Remember where the blob starts in it: the caller may hand us a
	// file positioned partway in.
	if seeker, ok := r.(io.Seeker); ok {
		if base, err := seeker.Seek(0, io.SeekCurrent); err == nil {
			upload.seeker, upload.base = seeker, base
		}
	}
	upload.newResourceName()
	return upload.run(ctx)
}

// byteStreamUpload is one resumable ByteStream write of a blob.
type byteStreamUpload struct {
	owner  *CAS
	digest Digest
	src    io.Reader
	// seeker is src when it can be rewound, and base is the offset the blob
	// starts at in it.
	seeker io.Seeker
	base   int64

	// resourceName carries the upload's identity. It changes only when the upload
	// has to start over from offset 0, so the server never sees two different
	// prefixes under one name.
	resourceName string

	// buf holds the chunk currently in flight: with a source we cannot rewind,
	// the only bytes we can still reproduce after a failure.
	buf        []byte
	chunkStart int64 // absolute offset of buf[0]
	chunkLen   int   // valid bytes in buf

	read      int64 // bytes consumed from src, always chunkStart+chunkLen
	committed int64 // bytes the server is known to have committed
	srcEOF    bool
	// noQuery records that the server does not implement QueryWriteStatus, so
	// there is no point asking again.
	noQuery bool
}

func (u *byteStreamUpload) newResourceName() {
	name := fmt.Sprintf("uploads/%s/blobs/%x/%d", uuid.NewString(), u.digest.Hash, u.digest.SizeBytes)
	if u.owner.instanceName != "" {
		name = u.owner.instanceName + "/" + name
	}
	u.resourceName = name
}

// run streams the blob, resuming after transient failures for as long as it can
// reproduce the bytes the server is missing.
func (u *byteStreamUpload) run(ctx context.Context) error {
	r := u.owner.retry.start(fmt.Sprintf("uploading blob %s (%d bytes) to the remote cache", u.digest.hexHash(), u.digest.SizeBytes))
	for {
		err := u.sendFrom(ctx)
		if err == nil {
			return nil
		}
		if giveUp := r.next(ctx, err); giveUp != nil {
			return casErr(giveUp)
		}
		complete, rewindErr := u.rewind(ctx)
		if complete {
			return nil
		}
		if rewindErr != nil {
			return casErr(fmt.Errorf("%w (cannot resume the upload: %v)", err, rewindErr))
		}
	}
}

// sendFrom writes everything from the server's committed offset to the end of
// the blob over a single ByteStream write stream.
//
// Unlike a read, this stays on the connection the upload started on: the upload
// id in the resource name is server state, and a pool member is a separate
// connection that may not reach the same backend.
func (u *byteStreamUpload) sendFrom(ctx context.Context) error {
	stream, err := u.owner.byteStreamClient.Write(ctx)
	if err != nil {
		return err
	}
	offset := u.committed
	for {
		chunk, err := u.chunkAt(offset)
		if err != nil {
			stream.CloseSend()
			return err
		}
		last := offset+int64(len(chunk)) >= u.digest.SizeBytes
		if len(chunk) == 0 && !last {
			stream.CloseSend()
			return fmt.Errorf("source ended after %d bytes, expected %d", offset, u.digest.SizeBytes)
		}
		if err := stream.Send(&bytestream.WriteRequest{
			ResourceName: u.resourceName,
			WriteOffset:  offset,
			FinishWrite:  last,
			Data:         chunk,
		}); err != nil {
			// The server closed the stream. Its status is on CloseAndRecv, so stop
			// sending and go read it rather than reporting this placeholder error.
			break
		}
		offset += int64(len(chunk))
		if last {
			break
		}
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return err
	}
	// A write that ends without error has stored the blob. committed_size is the
	// blob size, or -1 when the server tells us somebody else finished the upload
	// first (bytestream.proto, Write).
	if resp.CommittedSize != u.digest.SizeBytes && resp.CommittedSize != -1 {
		return fmt.Errorf("remote cache committed %d bytes of blob %s, expected %d",
			resp.CommittedSize, u.digest.hexHash(), u.digest.SizeBytes)
	}
	return nil
}

// chunkAt returns the bytes to send at the given absolute offset: the retained
// tail of the chunk in flight when resuming inside it, otherwise the next chunk
// read from the source.
func (u *byteStreamUpload) chunkAt(offset int64) ([]byte, error) {
	switch {
	case offset < u.chunkStart:
		// rewind guarantees this cannot happen.
		return nil, fmt.Errorf("cannot re-send blob %s from offset %d: only %d onwards is still buffered",
			u.digest.hexHash(), offset, u.chunkStart)
	case offset < u.chunkStart+int64(u.chunkLen):
		return u.buf[offset-u.chunkStart : u.chunkLen], nil
	case u.srcEOF:
		return nil, nil
	}
	n, err := io.ReadFull(u.src, u.buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("reading blob data: %w", err)
	}
	if err != nil {
		u.srcEOF = true
	}
	u.chunkStart = offset
	u.chunkLen = n
	u.read = offset + int64(n)
	return u.buf[:n], nil
}

// rewind asks the server how much of the upload it committed and positions the
// next attempt there. It reports whether the blob turned out to be complete
// already, and fails when the server is behind bytes we can no longer reproduce.
func (u *byteStreamUpload) rewind(ctx context.Context) (bool, error) {
	if u.noQuery {
		return false, u.restart()
	}
	committed, err := u.queryWriteStatus(ctx)
	switch {
	case status.Code(err) == codes.Unimplemented:
		// Some servers do not implement QueryWriteStatus; remember that and start
		// over instead of asking again on every retry (as Bazel does).
		u.noQuery = true
		return false, u.restart()
	case status.Code(err) == codes.NotFound:
		// The upload the server knew about is gone, so nothing is committed.
		return false, u.restart()
	case err != nil:
		return false, err
	}
	if committed < 0 || committed >= u.digest.SizeBytes {
		// The whole blob is committed: either another client finished it (-1) or we
		// only lost the acknowledgement of our own last chunk.
		return true, nil
	}
	// Resuming means sending from committed onwards. That works as long as the
	// bytes are either still in buf or still ahead of us in the source.
	if (committed < u.chunkStart || committed > u.read) && !u.rewindTo(committed) {
		return false, fmt.Errorf("remote cache committed %d bytes of blob %s, and the source cannot be rewound to it (bytes %d to %d are what we still hold)",
			committed, u.digest.hexHash(), u.chunkStart, u.read)
	}
	u.committed = committed
	return false, nil
}

// restart prepares a retry from offset 0, which needs either a source we can
// rewind or everything read so far still buffered.
func (u *byteStreamUpload) restart() error {
	if u.chunkStart > 0 && !u.rewindTo(0) {
		return fmt.Errorf("cannot restart the upload of blob %s: %d bytes of the source are already consumed and it cannot be rewound",
			u.digest.hexHash(), u.read)
	}
	u.committed = 0
	// A fresh upload id, so a server that did commit part of the previous attempt
	// never sees offset 0 written twice under one resource name.
	u.newResourceName()
	return nil
}

// rewindTo puts the source back at the given blob offset, reporting whether it
// could. Everything buffered is dropped: the next chunk is read from there.
func (u *byteStreamUpload) rewindTo(offset int64) bool {
	if u.seeker == nil {
		return false
	}
	if _, err := u.seeker.Seek(u.base+offset, io.SeekStart); err != nil {
		return false
	}
	u.chunkStart, u.chunkLen, u.read, u.srcEOF = offset, 0, offset, false
	return true
}

func (u *byteStreamUpload) queryWriteStatus(ctx context.Context) (int64, error) {
	callCtx, cancel := u.owner.callContext(ctx)
	defer cancel()
	resp, err := u.owner.byteStreamClient.QueryWriteStatus(callCtx, &bytestream.QueryWriteStatusRequest{
		ResourceName: u.resourceName,
	})
	if err != nil {
		return 0, err
	}
	if resp.Complete {
		return u.digest.SizeBytes, nil
	}
	return resp.CommittedSize, nil
}
