package compactstream

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// BlobRequest identifies one content-addressed blob a compact stream references.
type BlobRequest struct {
	Digest []byte
	Size   int64
}

// MultiBlobStore is an optional extension of [BlobStore] for stores that can
// serve a whole list of blobs at once. ReaderForBlobs returns the concatenation
// of the requested blobs, in request order, so reading it is indistinguishable
// from reading each blob with ReaderForBlob in turn.
//
// Reconstruction hands the store the entire reference table of a layer up front,
// which is what lets a store batch. A layer that references thousands of small
// files costs one round trip per file when each is fetched on its own; a store
// that sees the whole list can ask a remote CAS for many blobs per request and
// pay a round trip per few megabytes instead.
//
// The result must be streamed, not materialized: the concatenation is as large
// as the layer's file contents, so an implementation may only ever hold a
// bounded part of it in memory.
type MultiBlobStore interface {
	BlobStore
	ReaderForBlobs(ctx context.Context, requests []BlobRequest) (io.ReadCloser, error)
}

// BlobStreamPart is one part of a concatenated blob stream: a reader that
// delivers exactly Size bytes, opened only once the stream reaches it.
//
// A part is one blob or a run of blobs the same source serves together --
// whatever unit an implementation wants to fetch in one go.
type BlobStreamPart struct {
	Size int64
	Open func() (io.ReadCloser, error)
}

// NewBlobStream returns a reader over the concatenation of parts. Each part is
// opened when the stream reaches it and closed before the next one is opened, so
// at most one part is in flight at a time. A part that ends before it delivered
// Size bytes fails the read; bytes past Size are ignored.
//
// It is the plumbing behind [MultiBlobStore]: reconstruction's own fallback is
// one part per blob, and a store that serves some blobs locally and batches the
// rest is one part per run.
func NewBlobStream(parts []BlobStreamPart) io.ReadCloser {
	return &blobStream{parts: parts}
}

// blobStreamFor returns a reader over the concatenation of the requested blobs,
// using the store's own [MultiBlobStore] implementation when it has one.
func blobStreamFor(ctx context.Context, store BlobStore, requests []BlobRequest) (io.ReadCloser, error) {
	if multi, ok := store.(MultiBlobStore); ok {
		return multi.ReaderForBlobs(ctx, requests)
	}
	return serialBlobStream(ctx, store, requests), nil
}

// serialBlobStream is the fallback for a plain [BlobStore]: one part per blob,
// each opened with ReaderForBlob when the stream reaches it.
func serialBlobStream(ctx context.Context, store BlobStore, requests []BlobRequest) io.ReadCloser {
	parts := make([]BlobStreamPart, len(requests))
	for i, request := range requests {
		parts[i] = BlobStreamPart{
			Size: request.Size,
			Open: func() (io.ReadCloser, error) {
				return store.ReaderForBlob(ctx, request.Digest, request.Size)
			},
		}
	}
	return NewBlobStream(parts)
}

// blobStream walks a list of parts, concatenating them into one stream.
type blobStream struct {
	parts []BlobStreamPart

	next      int           // index of the part to open next
	cur       io.ReadCloser // the open part, nil between parts
	remaining int64         // bytes the open part still owes us
	err       error         // sticky: the stream delivers nothing after a failure
}

func (s *blobStream) Read(p []byte) (int, error) {
	for {
		if s.err != nil {
			return 0, s.err
		}
		if s.cur == nil {
			if s.next >= len(s.parts) {
				return 0, io.EOF
			}
			part := s.parts[s.next]
			s.next++
			if part.Size <= 0 {
				continue // an empty part contributes nothing
			}
			reader, err := part.Open()
			if err != nil {
				s.err = err
				return 0, s.err
			}
			s.cur = reader
			s.remaining = part.Size
		}
		if len(p) == 0 {
			return 0, nil
		}

		// Never read past the end of the part: the reader may be shared with
		// whatever comes after it.
		buf := p
		if int64(len(buf)) > s.remaining {
			buf = buf[:s.remaining]
		}
		n, err := s.cur.Read(buf)
		s.remaining -= int64(n)

		switch {
		case s.remaining == 0:
			// The part is complete. Anything it has left over is not ours to read.
			if closeErr := s.closeCurrent(); closeErr != nil {
				s.err = closeErr
			}
		case err != nil:
			s.err = s.partError(err)
			s.closeCurrent()
		}

		if n > 0 {
			return n, nil
		}
		if s.err != nil {
			return 0, s.err
		}
		if s.cur != nil {
			// The part returned no bytes and no error; let the caller come back
			// rather than spinning here.
			return 0, nil
		}
	}
}

// partError describes a part that failed before delivering everything it owed.
func (s *blobStream) partError(err error) error {
	part := s.parts[s.next-1]
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("blob stream part %d of %d ended %d bytes short of its %d bytes: %w",
			s.next, len(s.parts), s.remaining, part.Size, io.ErrUnexpectedEOF)
	}
	return fmt.Errorf("reading blob stream part %d of %d: %w", s.next, len(s.parts), err)
}

func (s *blobStream) closeCurrent() error {
	if s.cur == nil {
		return nil
	}
	err := s.cur.Close()
	s.cur = nil
	return err
}

func (s *blobStream) Close() error {
	s.next = len(s.parts)
	return s.closeCurrent()
}
