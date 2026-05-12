package tarcas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/klauspost/compress/zstd"
)

type BlobStore interface {
	ReaderForBlob(ctx context.Context, digest []byte, size int64) (io.ReadCloser, error)
}

type LocalBlobResolver interface {
	ResolveLocalBlob(path string, digest []byte, newHash func() hash.Hash, size int64) (io.ReadCloser, bool)
}

type FSLocalBlobResolver struct{}

func (FSLocalBlobResolver) ResolveLocalBlob(path string, digest []byte, newHash func() hash.Hash, size int64) (io.ReadCloser, bool) {
	if path == "" {
		return nil, false
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}

	info, err := f.Stat()
	if err != nil || info.Size() != size {
		f.Close()
		return nil, false
	}

	h := newHash()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return nil, false
	}
	if !bytes.Equal(h.Sum(nil), digest) {
		f.Close()
		return nil, false
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, false
	}

	return f, true
}

func ReconstructFromIndex(ctx context.Context, index io.Reader, store BlobStore, localResolver LocalBlobResolver, output io.Writer) error {
	var fileHeader [indexHeaderSize]byte
	if _, err := io.ReadFull(index, fileHeader[:]); err != nil {
		return fmt.Errorf("reading index file header: %w", err)
	}

	if string(fileHeader[0:6]) != indexMagic {
		return fmt.Errorf("invalid index magic: %q", fileHeader[0:6])
	}
	if fileHeader[6] != 0x00 {
		return fmt.Errorf("expected NUL byte at offset 6, got %x", fileHeader[6])
	}
	if fileHeader[7] != indexVersion {
		return fmt.Errorf("unsupported index version: %d", fileHeader[7])
	}

	hashSize := int(binary.BigEndian.Uint16(fileHeader[10:12]))
	hashAlgo := binary.BigEndian.Uint16(fileHeader[8:10])
	streamCompression := fileHeader[12]
	originalCompression := fileHeader[13]
	seekableCompression := fileHeader[14] == 1
	compressionLevel := int8(fileHeader[15])
	compressorJobs := fileHeader[16]
	endPadding := binary.BigEndian.Uint32(fileHeader[20:24])
	refTableOffset := binary.BigEndian.Uint64(fileHeader[24:32])
	refTableSize := binary.BigEndian.Uint64(fileHeader[32:40])
	_ = binary.BigEndian.Uint64(fileHeader[40:48]) // streamOffset (known from layout)
	_ = binary.BigEndian.Uint64(fileHeader[48:56]) // streamSize
	localPathTableOffset := binary.BigEndian.Uint64(fileHeader[56:64])
	localPathTableSize := binary.BigEndian.Uint64(fileHeader[64:72])

	_ = refTableOffset // we read sequentially after the header
	_ = hashSize

	var newHash func() hash.Hash
	switch hashAlgo {
	case IndexHashAlgoSHA256:
		newHash = sha256.New
	default:
		return fmt.Errorf("unsupported hash algorithm: %d", hashAlgo)
	}

	refEntrySize := 16 + hashSize
	if refTableSize > 0 && refTableSize%uint64(refEntrySize) != 0 {
		return fmt.Errorf("ref table size %d is not a multiple of entry size %d", refTableSize, refEntrySize)
	}
	refCount := int(refTableSize / uint64(refEntrySize))

	refTableData := make([]byte, refTableSize)
	if _, err := io.ReadFull(index, refTableData); err != nil {
		return fmt.Errorf("reading CAS reference table: %w", err)
	}

	type ref struct {
		offset        uint64
		digest        []byte
		size          uint64
		localPathHint string
	}
	refs := make([]ref, refCount)
	for i := range refs {
		base := i * refEntrySize
		refs[i].offset = binary.BigEndian.Uint64(refTableData[base : base+8])
		refs[i].digest = refTableData[base+8 : base+8+hashSize]
		refs[i].size = binary.BigEndian.Uint64(refTableData[base+8+hashSize : base+refEntrySize])
	}

	// Read local path table if present
	if localPathTableOffset > 0 && localPathTableSize > 0 {
		localPathData := make([]byte, localPathTableSize)
		if _, err := io.ReadFull(index, localPathData); err != nil {
			return fmt.Errorf("reading local path table: %w", err)
		}
		pathIdx := 0
		for i := range refs {
			end := bytes.IndexByte(localPathData[pathIdx:], 0)
			if end < 0 {
				return fmt.Errorf("local path table: missing null terminator for entry %d", i)
			}
			refs[i].localPathHint = string(localPathData[pathIdx : pathIdx+end])
			pathIdx += end + 1
		}
	}

	var stream io.Reader
	switch streamCompression {
	case IndexStreamCompressionNone:
		stream = index
	case IndexStreamCompressionZstd:
		dec, err := zstd.NewReader(index)
		if err != nil {
			return fmt.Errorf("creating zstd decoder for stream: %w", err)
		}
		defer dec.Close()
		stream = dec
	default:
		return fmt.Errorf("unsupported stream compression: %d", streamCompression)
	}

	var reconstructed bytes.Buffer

	var outputPos uint64
	for _, r := range refs {
		gap := r.offset - outputPos
		if gap > 0 {
			if _, err := io.CopyN(&reconstructed, stream, int64(gap)); err != nil {
				return fmt.Errorf("copying stream bytes at offset %d (gap %d): %w", outputPos, gap, err)
			}
		}

		var blobReader io.ReadCloser
		if localResolver != nil && r.localPathHint != "" {
			if rc, ok := localResolver.ResolveLocalBlob(r.localPathHint, r.digest, newHash, int64(r.size)); ok {
				blobReader = rc
			}
		}
		if blobReader == nil {
			var err error
			blobReader, err = store.ReaderForBlob(ctx, r.digest, int64(r.size))
			if err != nil {
				return fmt.Errorf("fetching blob at offset %d: %w", r.offset, err)
			}
		}
		if _, err := io.CopyN(&reconstructed, blobReader, int64(r.size)); err != nil {
			blobReader.Close()
			return fmt.Errorf("reading blob at offset %d: %w", r.offset, err)
		}
		blobReader.Close()

		outputPos = r.offset + r.size
	}

	if _, err := io.Copy(&reconstructed, stream); err != nil {
		return fmt.Errorf("copying remaining stream bytes: %w", err)
	}

	if originalCompression == IndexOriginalCompressionNone {
		if _, err := reconstructed.WriteTo(output); err != nil {
			return fmt.Errorf("writing uncompressed output: %w", err)
		}
	} else {
		var compressionAlgorithm string
		switch originalCompression {
		case IndexOriginalCompressionGzip:
			compressionAlgorithm = "gzip"
		case IndexOriginalCompressionZstd:
			compressionAlgorithm = "zstd"
		default:
			return fmt.Errorf("unsupported original compression: %d", originalCompression)
		}

		var compressOpts []compress.Option
		if compressionLevel >= 0 {
			compressOpts = append(compressOpts, compress.CompressionLevel(compressionLevel))
		}
		if compressorJobs > 0 {
			compressOpts = append(compressOpts, compress.CompressorJobs(int(compressorJobs)))
		}

		appender, err := compress.TarAppenderFactory("sha256", compressionAlgorithm, seekableCompression, output, compressOpts...)
		if err != nil {
			return fmt.Errorf("creating compressor: %w", err)
		}

		if err := appender.AppendTar(&reconstructed); err != nil {
			return fmt.Errorf("compressing output: %w", err)
		}

		if _, err := appender.Finalize(); err != nil {
			return fmt.Errorf("finalizing compressor: %w", err)
		}
	}

	if endPadding > 0 {
		padding := make([]byte, endPadding)
		if _, err := output.Write(padding); err != nil {
			return fmt.Errorf("writing end padding: %w", err)
		}
	}

	return nil
}
