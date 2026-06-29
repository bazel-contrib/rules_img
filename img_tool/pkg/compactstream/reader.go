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
