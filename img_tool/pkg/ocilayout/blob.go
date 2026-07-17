package ocilayout

import (
	"bytes"
	"context"
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Blob is a tagged union describing the origin of a single blob's content.
// Exactly one of Bytes, Path or Open must be set; this is enforced by the sink
// at write time via (Blob).validate.
//
// The same Blob feeds a directory sink (which hardlinks/reflinks Path blobs)
// and a tar sink (which streams every blob), so callers never decide how a
// blob is materialized — the sink does.
type Blob struct {
	// Bytes is in-memory content (e.g. a manifest/config/index JSON already
	// loaded). Size is derived from it automatically.
	Bytes []byte
	// Path is a local file path. On a directory sink it enables
	// hardlink/reflink/symlink; on a tar sink it is opened and streamed.
	Path string
	// Open streams the content (e.g. from a VFS/registry/CAS). It is never
	// buffered fully in memory. Size must be set when Open is used because tar
	// headers require the size up front.
	Open func(ctx context.Context) (io.ReadCloser, int64, error)
	// Size is the content length. Required for Open; derived for Bytes; and
	// stat'd lazily by the sink for Path.
	Size int64
}

// BlobFromBytes returns a Blob backed by in-memory bytes.
func BlobFromBytes(b []byte) Blob { return Blob{Bytes: b, Size: int64(len(b))} }

// BlobFromPath returns a Blob backed by a local file path.
func BlobFromPath(p string) Blob { return Blob{Path: p} }

// BlobFromSource adapts a streaming BlobSource into a Blob. The size must be
// known ahead of time (tar headers need it); layer/config descriptors already
// carry it. This is how the load pipeline reuses its VFS-backed blob source.
func BlobFromSource(src BlobSource, hexDigest string, size int64) Blob {
	return Blob{
		Size: size,
		Open: func(ctx context.Context) (io.ReadCloser, int64, error) {
			return src.OpenBlob(ctx, hexDigest)
		},
	}
}

func (b Blob) isZero() bool { return b.Bytes == nil && b.Path == "" && b.Open == nil }

// reader opens the blob content along with its size. The caller must close the
// returned ReadCloser.
func (b Blob) reader(ctx context.Context) (io.ReadCloser, int64, error) {
	switch {
	case b.Bytes != nil:
		return io.NopCloser(bytes.NewReader(b.Bytes)), int64(len(b.Bytes)), nil
	case b.Open != nil:
		return b.Open(ctx)
	default:
		// Path-backed blobs are opened by the sink itself (it may stat/link
		// the file directly); reader is only used for the streaming path.
		return nil, 0, errNoBlobContent
	}
}

// BlobSource is the streaming blob interface. It is intentionally identical to
// the interface the load pipeline's VFS already implements, so that source
// migrates without changes.
type BlobSource interface {
	OpenBlob(ctx context.Context, hexDigest string) (io.ReadCloser, int64, error)
}

// PathBlobSource is an optional capability: a BlobSource that can also expose a
// local file path for a blob, enabling hardlink/reflink on directory sinks.
type PathBlobSource interface {
	BlobSource
	BlobPath(hexDigest string) (path string, ok bool)
}

// FileBlobSource maps blob digests to local file paths. It implements
// PathBlobSource and replaces the per-command blobMap constructions.
type FileBlobSource struct {
	paths map[string]string
}

// NewFileBlobSource returns an empty FileBlobSource.
func NewFileBlobSource() *FileBlobSource {
	return &FileBlobSource{paths: make(map[string]string)}
}

// Add registers a file path for a blob digest (hex, without the "sha256:"
// prefix) and returns the source for chaining.
func (s *FileBlobSource) Add(hexDigest, path string) *FileBlobSource {
	s.paths[hexDigest] = path
	return s
}

// BlobPath implements PathBlobSource.
func (s *FileBlobSource) BlobPath(hexDigest string) (string, bool) {
	p, ok := s.paths[hexDigest]
	return p, ok
}

// OpenBlob implements BlobSource.
func (s *FileBlobSource) OpenBlob(_ context.Context, hexDigest string) (io.ReadCloser, int64, error) {
	p, ok := s.paths[hexDigest]
	if !ok {
		return nil, 0, errBlobNotFound(hexDigest)
	}
	return openFileBlob(p)
}

// MemBlobSource maps blob digests to in-memory bytes. Useful for tests and for
// callers that already hold the content.
type MemBlobSource struct {
	blobs map[string][]byte
}

// NewMemBlobSource returns an empty MemBlobSource.
func NewMemBlobSource() *MemBlobSource {
	return &MemBlobSource{blobs: make(map[string][]byte)}
}

// Add registers bytes for a blob digest and returns the source for chaining.
func (s *MemBlobSource) Add(hexDigest string, data []byte) *MemBlobSource {
	s.blobs[hexDigest] = data
	return s
}

// OpenBlob implements BlobSource.
func (s *MemBlobSource) OpenBlob(_ context.Context, hexDigest string) (io.ReadCloser, int64, error) {
	data, ok := s.blobs[hexDigest]
	if !ok {
		return nil, 0, errBlobNotFound(hexDigest)
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// hashBytes returns the SHA-256 of data as a v1.Hash. This is the single copy
// that replaces the per-package duplicates.
func hashBytes(data []byte) v1.Hash {
	h, _, _ := v1.SHA256(bytes.NewReader(data))
	return h
}
