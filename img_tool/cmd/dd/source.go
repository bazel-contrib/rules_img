package ddcmd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/extentstream"
)

// source is something the bytes of an image can be read from.
type source interface {
	// size is the size of the image, or -1 if it is not known.
	size() int64
	// copyRange writes n bytes (n < 0: until the end) from offset off to out.
	copyRange(ctx context.Context, out io.Writer, off, n int64) error
	close() error
}

// openSource opens a raw image, an extent stream or a compact stream. The
// store is only used for streams, and only created when it is needed.
func openSource(path string, store func() (compactstream.BlobStore, error)) (source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	magic := make([]byte, 8)
	n, _ := io.ReadFull(f, magic)
	magic = magic[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	switch {
	case extentstream.HasMagic(magic):
		idx, err := extentstream.Inspect(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		st, err := store()
		if err != nil {
			f.Close()
			return nil, err
		}
		return &extentSource{f: f, store: st, sz: int64(idx.Header.Size)}, nil
	case compactstream.HasMagic(magic):
		info, err := compactstream.Inspect(io.NopCloser(f))
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		sz := int64(-1)
		switch {
		case info.Header.OriginalCompression.Compression == compactstream.OriginalCompressionNone:
			sz = int64(info.ReconstructedSize()) + int64(info.Header.OriginalCompression.EndPadding)
		case info.Header.HasCompressedStreamInfo:
			sz = int64(info.Header.CompressedStreamSize)
		}
		st, err := store()
		if err != nil {
			f.Close()
			return nil, err
		}
		return &cstreamSource{f: f, store: st, sz: sz}, nil
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &rawSource{f: f, sz: st.Size()}, nil
}

type rawSource struct {
	f  *os.File
	sz int64
}

func (s *rawSource) size() int64  { return s.sz }
func (s *rawSource) close() error { return s.f.Close() }

func (s *rawSource) copyRange(_ context.Context, out io.Writer, off, n int64) error {
	if n < 0 || off+n > s.sz {
		n = s.sz - off
	}
	_, err := io.Copy(out, io.NewSectionReader(s.f, off, n))
	return err
}

type extentSource struct {
	f     *os.File
	store compactstream.BlobStore
	sz    int64
}

func (s *extentSource) size() int64  { return s.sz }
func (s *extentSource) close() error { return s.f.Close() }

func (s *extentSource) copyRange(ctx context.Context, out io.Writer, off, n int64) error {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return extentstream.ReconstructRange(ctx, s.f, s.store, out, off, n)
}

type cstreamSource struct {
	f     *os.File
	store compactstream.BlobStore
	sz    int64
}

func (s *cstreamSource) size() int64  { return s.sz }
func (s *cstreamSource) close() error { return s.f.Close() }

func (s *cstreamSource) copyRange(ctx context.Context, out io.Writer, off, n int64) error {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	rr, err := compactstream.NewReconstructingReader(ctx, io.NopCloser(s.f), s.store)
	if err != nil {
		return err
	}
	defer rr.Close()
	if _, err := io.CopyN(io.Discard, rr, off); err != nil {
		return fmt.Errorf("skipping %d bytes: %w", off, err)
	}
	var r io.Reader = rr
	if n >= 0 {
		r = io.LimitReader(rr, n)
	}
	_, err = io.Copy(out, r)
	return err
}

// blobSource is an image that is only known by its digest and size: it is read
// from the store, like the blobs a stream references. This is how an image that
// is a plain file is written lazily.
type blobSource struct {
	store  compactstream.BlobStore
	digest []byte
	sz     int64
}

// openBlobSource reads a descriptor: a JSON file with a "digest" (sha256:<hex>)
// and a "size", such as the metadata file of a layer.
func openBlobSource(path string, store func() (compactstream.BlobStore, error)) (source, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var desc struct {
		Digest string `json:"digest"`
		Size   *int64 `json:"size"`
	}
	if err := json.Unmarshal(data, &desc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	hexDigest, ok := strings.CutPrefix(desc.Digest, "sha256:")
	digest, err := hex.DecodeString(hexDigest)
	if !ok || err != nil || len(digest) != 32 || desc.Size == nil || *desc.Size < 0 {
		return nil, fmt.Errorf("%s: want a sha256 \"digest\" and a \"size\"", path)
	}
	st, err := store()
	if err != nil {
		return nil, err
	}
	return &blobSource{store: st, digest: digest, sz: *desc.Size}, nil
}

func (s *blobSource) size() int64  { return s.sz }
func (s *blobSource) close() error { return nil }

func (s *blobSource) copyRange(ctx context.Context, out io.Writer, off, n int64) error {
	if n < 0 || off+n > s.sz {
		n = s.sz - off
	}
	rc, err := s.store.ReaderForBlob(ctx, s.digest, s.sz)
	if err != nil {
		return err
	}
	defer rc.Close()
	if sk, ok := rc.(io.Seeker); ok {
		_, err = sk.Seek(off, io.SeekStart)
	} else {
		_, err = io.CopyN(io.Discard, rc, off)
	}
	if err != nil {
		return fmt.Errorf("skipping %d bytes: %w", off, err)
	}
	_, err = io.Copy(out, io.LimitReader(rc, n))
	return err
}
