package compactstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
)

type BlobStore interface {
	ReaderForBlob(ctx context.Context, digest []byte, size int64) (io.ReadCloser, error)
}

func Reconstruct(ctx context.Context, index io.Reader, store BlobStore, output io.Writer) error {
	header, err := ReadHeader(index)
	if err != nil {
		return err
	}

	refs, err := readRefTable(index, header)
	if err != nil {
		return err
	}

	stream, err := streamReader(index, header.StreamCompression)
	if err != nil {
		return err
	}
	if closer, ok := stream.(io.Closer); ok {
		defer closer.Close()
	}

	// When the index records the digest and size of the reconstructed compressed
	// stream, tee everything we write to output through a hash so we can validate
	// the result matches what the index promises.
	out := output
	var verifier *hashCountWriter
	if header.HasCompressedStreamInfo {
		verifier = &hashCountWriter{w: output, h: sha256.New()}
		out = verifier
	}

	origComp := header.OriginalCompression
	if origComp.Compression == OriginalCompressionNone {
		// No re-compression: stream the reconstructed bytes straight to out.
		if err := writeReconstructed(ctx, out, stream, refs, store); err != nil {
			return err
		}
	} else {
		var compressionAlgorithm string
		switch origComp.Compression {
		case OriginalCompressionGzip:
			compressionAlgorithm = "gzip"
		case OriginalCompressionZstd:
			compressionAlgorithm = "zstd"
		default:
			return fmt.Errorf("unsupported original compression: %d", origComp.Compression)
		}

		var compressOpts []compress.Option
		if origComp.CompressionLevel >= 0 {
			compressOpts = append(compressOpts, compress.CompressionLevel(int(origComp.CompressionLevel)))
		}
		if origComp.CompressorJobs > 0 {
			compressOpts = append(compressOpts, compress.CompressorJobs(int(origComp.CompressorJobs)))
		}

		appender, err := compress.TarAppenderFactory("sha256", compressionAlgorithm, origComp.Seekable, out, compressOpts...)
		if err != nil {
			return fmt.Errorf("creating compressor: %w", err)
		}

		// Feed the reconstructed (uncompressed) tar through a pipe into the
		// compressor so the whole tar is never held in memory: peak usage stays
		// O(copy buffer + compressor window) instead of O(uncompressed layer).
		pr, pw := io.Pipe()
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			pw.CloseWithError(writeReconstructed(ctx, pw, stream, refs, store))
		}()

		appendErr := appender.AppendTar(pr)
		if appendErr != nil {
			// Unblock the writer goroutine if it is still trying to write.
			pr.CloseWithError(appendErr)
		}
		// Wait for the writer goroutine to finish before returning. Closing pr
		// above only unblocks a pending write, not an in-flight read of `stream`,
		// so we must not let the deferred stream Close() (e.g. the zstd decoder,
		// which is not safe to close concurrently with a read) run until the
		// goroutine has stopped reading.
		<-writerDone
		if appendErr != nil {
			return fmt.Errorf("compressing output: %w", appendErr)
		}

		if _, err := appender.Finalize(); err != nil {
			return fmt.Errorf("finalizing compressor: %w", err)
		}
	}

	if origComp.EndPadding > 0 {
		padding := make([]byte, origComp.EndPadding)
		if _, err := out.Write(padding); err != nil {
			return fmt.Errorf("writing end padding: %w", err)
		}
	}

	if verifier != nil {
		gotDigest := verifier.h.Sum(nil)
		if verifier.n != int64(header.CompressedStreamSize) || !bytes.Equal(gotDigest, header.CompressedStreamDigest) {
			return fmt.Errorf("reconstructed compressed stream mismatch: expected a compressed stream with digest %s and size %d, but reconstructed a stream with digest %s and size %d",
				hex.EncodeToString(header.CompressedStreamDigest), header.CompressedStreamSize,
				hex.EncodeToString(gotDigest), verifier.n)
		}
	}

	return nil
}

// ReconstructingReader is an io.Reader over the *uncompressed* layer tar
// reconstructed from a compact stream: it interleaves the decompressed byte
// stream (tar headers, inlined small files, and block padding) with the blobs
// supplied by store at their recorded offsets. Pair it with NullBlobStore to
// zero-fill the CAS-referenced content when the content store is unavailable, so
// a standard archive/tar reader can still walk every header.
//
// It additionally tracks the current output offset and can report the digest a
// compact stream recorded for a file's content, which lets a consumer attach
// content digests to tar entries without a content store: for a CAS-referenced
// file, RefDigestAt returns the recorded digest (the sha256 of the file content);
// for an inlined file the content is present verbatim in the stream and can be
// hashed by reading it through this reader.
type ReconstructingReader struct {
	ctx           context.Context
	stream        io.Reader
	streamCloser  io.Closer
	refs          []CASReference
	refByOffset   map[uint64]CASReference
	store         BlobStore
	outputPos     uint64
	refIdx        int
	blob          io.ReadCloser
	blobRemaining int64
	err           error
}

// NewReconstructingReader reads and validates the compact stream header and CAS
// reference table from index, leaving index positioned at the byte stream, and
// returns a reader that reconstructs the uncompressed tar on demand.
func NewReconstructingReader(ctx context.Context, index io.Reader, store BlobStore) (*ReconstructingReader, error) {
	header, err := ReadHeader(index)
	if err != nil {
		return nil, err
	}
	refs, err := readRefTable(index, header)
	if err != nil {
		return nil, err
	}
	stream, err := streamReader(index, header.StreamCompression)
	if err != nil {
		return nil, err
	}
	refByOffset := make(map[uint64]CASReference, len(refs))
	for _, ref := range refs {
		refByOffset[ref.Offset] = ref
	}
	r := &ReconstructingReader{
		ctx:         ctx,
		stream:      stream,
		refs:        refs,
		refByOffset: refByOffset,
		store:       store,
	}
	if closer, ok := stream.(io.Closer); ok {
		r.streamCloser = closer
	}
	return r, nil
}

// Offset returns the number of reconstructed (uncompressed) tar bytes produced so
// far. After archive/tar's Reader.Next() returns, it equals the byte offset of
// the current entry's content in the uncompressed tar, which is the key against
// which RefDigestAt is queried.
func (r *ReconstructingReader) Offset() int64 { return int64(r.outputPos) }

// RefDigestAt reports the digest a CAS reference recorded for the content range
// starting at offset and spanning size bytes, if such a reference exists. The
// digest is the sha256 of the file content. It returns (nil, false) when the
// content is not CAS-referenced (e.g. an inlined small file), in which case the
// caller should hash the content read through this reader instead.
func (r *ReconstructingReader) RefDigestAt(offset, size int64) ([]byte, bool) {
	ref, ok := r.refByOffset[uint64(offset)]
	if !ok || int64(ref.Size) != size {
		return nil, false
	}
	return ref.Digest, true
}

func (r *ReconstructingReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if len(p) == 0 {
		return 0, nil
	}

	// Continue serving the current CAS blob region, if any.
	if r.blobRemaining > 0 {
		want := int64(len(p))
		if want > r.blobRemaining {
			want = r.blobRemaining
		}
		n, err := r.blob.Read(p[:want])
		r.outputPos += uint64(n)
		r.blobRemaining -= int64(n)
		if r.blobRemaining == 0 {
			closeErr := r.blob.Close()
			r.blob = nil
			if err == io.EOF {
				err = nil
			}
			if err == nil {
				err = closeErr
			}
		} else if err != nil {
			if err == io.EOF {
				err = fmt.Errorf("blob ended %d bytes short of its recorded size", r.blobRemaining)
			}
			r.err = err
		}
		return n, err
	}

	// Start serving a CAS blob if a reference begins at the current offset.
	if r.refIdx < len(r.refs) && r.refs[r.refIdx].Offset == r.outputPos {
		ref := r.refs[r.refIdx]
		r.refIdx++
		blob, err := r.store.ReaderForBlob(r.ctx, ref.Digest, int64(ref.Size))
		if err != nil {
			r.err = err
			return 0, err
		}
		r.blob = blob
		r.blobRemaining = int64(ref.Size)
		return r.Read(p)
	}

	// Serve inline byte-stream bytes, capped so we stop exactly at the next
	// reference boundary.
	want := int64(len(p))
	if r.refIdx < len(r.refs) {
		if gap := int64(r.refs[r.refIdx].Offset - r.outputPos); gap >= 0 && gap < want {
			want = gap
		}
	}
	n, err := r.stream.Read(p[:want])
	r.outputPos += uint64(n)
	return n, err
}

// Close releases the byte-stream decoder and any in-flight CAS blob reader.
func (r *ReconstructingReader) Close() error {
	var err error
	if r.blob != nil {
		err = r.blob.Close()
		r.blob = nil
	}
	if r.streamCloser != nil {
		if cerr := r.streamCloser.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// ReconstructUncompressed rebuilds the *uncompressed* layer tar from a compact
// stream by interleaving the decompressed byte stream with the blobs supplied
// by store, writing the raw tar bytes to output. Unlike Reconstruct it performs
// no re-compression and does not validate the compressed-stream digest: the
// output is the plain tar, not the original compressed file.
//
// Pair it with NullBlobStore to recover only the tar structure (all headers,
// any inlined small files, and tar block padding) when the content-addressed
// blobs are unavailable; the omitted blob ranges are then filled with the right
// number of zero bytes, so a standard archive/tar reader can walk every header
// (skipping the zeroed bodies) without access to the content store.
func ReconstructUncompressed(ctx context.Context, index io.Reader, store BlobStore, output io.Writer) error {
	r, err := NewReconstructingReader(ctx, index, store)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(output, r)
	return err
}

// NullBlobStore is a BlobStore that returns an equal-length run of NUL bytes for
// every requested blob, ignoring the digest. It lets ReconstructUncompressed
// recover the tar structure from a compact stream alone, without the
// content-addressed store: each omitted blob is replaced by zeros of the same
// size, yielding a valid tar whose file bodies are zeroed. This is sufficient to
// read all metadata (headers, link targets, sizes, modes, ...) with archive/tar,
// but the file contents are not recoverable.
type NullBlobStore struct{}

func (NullBlobStore) ReaderForBlob(_ context.Context, _ []byte, size int64) (io.ReadCloser, error) {
	return io.NopCloser(io.LimitReader(zeroReader{}, size)), nil
}

// zeroReader is an infinite source of NUL bytes. It zeroes the caller's buffer
// (which may hold stale data from a previous read) and reports it fully read.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// writeReconstructed writes the reconstructed (uncompressed) byte stream to dst
// by interleaving the on-disk byte-stream gaps with the CAS-referenced blobs in
// offset order. It streams the result: memory use is O(copy buffer), so it never
// materializes the whole tar.
func writeReconstructed(ctx context.Context, dst io.Writer, stream io.Reader, refs []CASReference, store BlobStore) error {
	var outputPos uint64
	for _, r := range refs {
		// readRefTable already rejects unsorted/overlapping refs; this guard is
		// belt-and-suspenders so the unsigned subtraction below can never
		// underflow into a negative int64 copy count.
		if r.Offset < outputPos {
			return fmt.Errorf("CAS ref offset %d precedes current output position %d (unsorted or overlapping ref table)", r.Offset, outputPos)
		}
		if gap := r.Offset - outputPos; gap > 0 {
			if _, err := io.CopyN(dst, stream, int64(gap)); err != nil {
				return fmt.Errorf("copying stream bytes at offset %d (gap %d): %w", outputPos, gap, err)
			}
		}

		blobReader, err := store.ReaderForBlob(ctx, r.Digest, int64(r.Size))
		if err != nil {
			return fmt.Errorf("fetching blob at offset %d: %w", r.Offset, err)
		}
		if _, err := io.CopyN(dst, blobReader, int64(r.Size)); err != nil {
			blobReader.Close()
			return fmt.Errorf("reading blob at offset %d: %w", r.Offset, err)
		}
		blobReader.Close()

		outputPos = r.Offset + r.Size
	}

	if _, err := io.Copy(dst, stream); err != nil {
		return fmt.Errorf("copying remaining stream bytes: %w", err)
	}
	return nil
}

// hashCountWriter forwards writes to an underlying writer while computing a
// running hash and byte count of everything written, used to validate the
// reconstructed compressed stream against the digest and size recorded in the
// index header.
type hashCountWriter struct {
	w io.Writer
	h hash.Hash
	n int64
}

func (hw *hashCountWriter) Write(p []byte) (int, error) {
	n, err := hw.w.Write(p)
	if n > 0 {
		hw.h.Write(p[:n])
		hw.n += int64(n)
	}
	return n, err
}
