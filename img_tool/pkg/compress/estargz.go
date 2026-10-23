package compress

import (
	"compress/gzip"
	"fmt"
	"hash"
	"io"
	"maps"

	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/estargz/zstdchunked"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// TarAppender appends tar entries to a compressed blob using estargz,
// while hashing the compressed and uncompressed data.
// It is resumable, meaning that it can be resumed from a previous state.
//
// The estargz writer rewrites the tar stream it is given (it re-encodes the
// entries and appends the TOC entry), so the content hash and the uncompressed
// size are taken from the stream estargz hands to the compressor, not from the
// tar stream that was appended.
type TarAppender[C TarCompressor] struct {
	hashFunctionName         string
	compressionAlgorithmName string
	outerHash                ResumableHash
	contentHash              CountingResumableHash
	compressor               C
	outputWriter             *countingWriter
	options
}

// NewTar creates a TarAppender with initial state.
func NewTar[C TarCompressor, HM hashMaker, CM tarCompressorMaker[C]](output io.Writer, opts ...Option) (TarAppender[C], error) {
	options := options{}
	for _, opt := range opts {
		opt.apply(&options)
	}

	var hashMaker HM
	outerHash := hashMaker.New()
	contentHash := newCountingHash(hashMaker.New(), 0)
	outputWriter := &countingWriter{w: output}

	var compressorMaker CM
	compress, err := setupTarWriterPipeline[C, CM](outputWriter, outerHash, contentHash, options)
	if err != nil {
		return TarAppender[C]{}, err
	}

	return TarAppender[C]{
		hashFunctionName:         hashMaker.Name(),
		compressionAlgorithmName: compressorMaker.Name(),
		outerHash:                outerHash,
		contentHash:              contentHash,
		compressor:               compress,
		outputWriter:             outputWriter,
		options:                  options,
	}, nil
}

// ResumeTar resumes the TarAppender from a previous state.
func ResumeTar[C TarCompressor, HM hashMaker, CM tarCompressorMaker[C]](state api.AppenderState, output io.Writer, opts ...Option) (TarAppender[C], error) {
	options := options{}
	for _, opt := range opts {
		opt.apply(&options)
	}

	var hashMaker HM
	outerHash := hashMaker.New()
	innerContentHash := hashMaker.New()
	if err := outerHash.UnmarshalBinary(state.OuterHashState); err != nil {
		return TarAppender[C]{}, err
	}
	if err := innerContentHash.UnmarshalBinary(state.ContentHashState); err != nil {
		return TarAppender[C]{}, err
	}
	contentHash := newCountingHash(innerContentHash, state.UncompressedSize)
	outputWriter := &countingWriter{w: output, n: state.CompressedSize}

	var compressorMaker CM
	compress, err := setupTarWriterPipeline[C, CM](outputWriter, outerHash, contentHash, options)
	if err != nil {
		return TarAppender[C]{}, err
	}

	appender := TarAppender[C]{
		hashFunctionName:         hashMaker.Name(),
		compressionAlgorithmName: compressorMaker.Name(),
		outerHash:                outerHash,
		contentHash:              contentHash,
		compressor:               compress,
		outputWriter:             outputWriter,
		options:                  options,
	}

	if state.Magic != appender.magic() {
		return TarAppender[C]{}, fmt.Errorf("magic mismatch: expected %s, got %s", appender.magic(), state.Magic)
	}

	return appender, nil
}

// AppendTar appends a tar entry to the TarAppender using estargz Writer.AppendTar.
func (a *TarAppender[C]) AppendTar(r io.Reader) error {
	return a.compressor.AppendTar(r)
}

// Finalize finalizes the TarAppender and returns the state.
func (a *TarAppender[C]) Finalize() (api.AppenderState, error) {
	// Closing the compressor flushes the data to the output
	// and ensures a trailer is written (if needed).
	if err := a.compressor.Close(); err != nil {
		return api.AppenderState{}, err
	}

	outerHashState, err := a.outerHash.MarshalBinary()
	if err != nil {
		return api.AppenderState{}, err
	}
	contentHashState, err := a.contentHash.MarshalBinary()
	if err != nil {
		return api.AppenderState{}, err
	}

	layerAnnotations := make(map[string]string)
	maps.Copy(layerAnnotations, a.compressor.Annotations())
	layerAnnotations[api.UncompressedSizeAnnotation] = fmt.Sprintf("%d", a.contentHash.BytesWritten())

	state := api.AppenderState{
		Magic:            a.magic(),
		OuterHashState:   outerHashState,
		OuterHash:        a.outerHash.Sum(nil),
		ContentHashState: contentHashState,
		ContentHash:      a.contentHash.Sum(nil),
		CompressedSize:   a.outputWriter.n,
		UncompressedSize: a.contentHash.BytesWritten(),
		LayerAnnotations: layerAnnotations,
	}
	return state, nil
}

func (a *TarAppender[C]) magic() string {
	magic := fmt.Sprintf("imgv1+tar+compressed+%s+%s", a.hashFunctionName, a.compressionAlgorithmName)
	if len(a.contentType) > 0 {
		magic += "+" + string(a.contentType)
	}
	return magic
}

func setupTarWriterPipeline[C TarCompressor, CM tarCompressorMaker[C]](output io.Writer, outerHash ResumableHash, contentHash CountingResumableHash, opts options) (C, error) {
	// Pipeline for writing data:
	// estargz writer -> output tee (to outerHash and final output)
	// the estargz writer also tees the tar stream it produces into contentHash
	outputTee := io.MultiWriter(output, outerHash)

	var compressorMaker CM
	var compress C
	if opts.compressionLevel != nil {
		var err error
		compress, err = compressorMaker.NewWriterLevel(outputTee, contentHash, int(*opts.compressionLevel))
		if err != nil {
			return compress, err
		}
	} else {
		compress = compressorMaker.NewWriter(outputTee, contentHash)
	}

	return compress, nil
}

// TarCompressor interface for estargz-based compression
type TarCompressor interface {
	AppendTar(r io.Reader) error
	Close() error
	Annotations() map[string]string
}

type tarCompressorMaker[T TarCompressor] interface {
	NewWriter(compressed io.Writer, contentHash CountingResumableHash) T
	NewWriterLevel(compressed io.Writer, contentHash CountingResumableHash, level int) (T, error)
	Name() string
}

// contentHashingCompressor wraps an estargz.Compressor so that every
// uncompressed byte the estargz writer emits is also written to contentHash.
//
// estargz re-encodes the tar entries it is given and appends its own
// stargz.index.json entry, so the stream that reaches the compressor - not the
// stream that was appended - is the layer's uncompressed content.
type contentHashingCompressor struct {
	estargz.Compressor
	contentHash CountingResumableHash
}

// Writer returns the compressing writer the estargz writer pushes the
// re-encoded tar entries into.
func (c contentHashingCompressor) Writer(w io.Writer) (estargz.WriteFlushCloser, error) {
	inner, err := c.Compressor.Writer(w)
	if err != nil {
		return nil, err
	}
	return contentHashingWriter{WriteFlushCloser: inner, contentHash: c.contentHash}, nil
}

// WriteTOCAndFooter writes the TOC. Compressors that put the TOC into the tar
// stream itself (gzip does, zstd:chunked does not) write it to diffHash as
// well, which is where the remaining uncompressed bytes show up.
func (c contentHashingCompressor) WriteTOCAndFooter(w io.Writer, off int64, toc *estargz.JTOC, diffHash hash.Hash) (digest.Digest, error) {
	return c.Compressor.WriteTOCAndFooter(w, off, toc, contentHashingHash{Hash: diffHash, contentHash: c.contentHash})
}

type contentHashingWriter struct {
	estargz.WriteFlushCloser
	contentHash CountingResumableHash
}

func (w contentHashingWriter) Write(p []byte) (int, error) {
	n, err := w.WriteFlushCloser.Write(p)
	if n > 0 {
		if _, hashErr := w.contentHash.Write(p[:n]); hashErr != nil && err == nil {
			err = hashErr
		}
	}
	return n, err
}

type contentHashingHash struct {
	hash.Hash
	contentHash CountingResumableHash
}

func (h contentHashingHash) Write(p []byte) (int, error) {
	if _, err := h.contentHash.Write(p); err != nil {
		return 0, err
	}
	return h.Hash.Write(p)
}

// EstargzWriter wraps estargz.Writer to implement TarCompressor
type EstargzWriter struct {
	writer            *estargz.Writer
	compressionFormat string
	tocDigest         string
}

// NewEstargzWriter creates a new EstargzWriter with default gzip compression
func NewEstargzWriter(compressed io.Writer, contentHash CountingResumableHash) *EstargzWriter {
	writer, _ := NewEstargzWriterLevel(compressed, contentHash, gzip.BestCompression)
	return writer
}

// NewEstargzWriterLevel creates a new EstargzWriter with compression level
func NewEstargzWriterLevel(compressed io.Writer, contentHash CountingResumableHash, level int) (*EstargzWriter, error) {
	return NewEstargzWriterWithCompression(compressed, contentHash, "gzip", level)
}

// NewEstargzWriterWithCompression creates a new EstargzWriter with specified compression
func NewEstargzWriterWithCompression(compressed io.Writer, contentHash CountingResumableHash, compressionFormat string, level int) (*EstargzWriter, error) {
	var compressor estargz.Compressor
	switch compressionFormat {
	case "gzip":
		compressor = estargz.NewGzipCompressorWithLevel(level)
	case "zstd":
		compressor = &zstdchunked.Compressor{
			CompressionLevel: zstd.EncoderLevel(level),
			Metadata:         make(map[string]string),
		}
	default:
		return nil, fmt.Errorf("unsupported compression format: %s", compressionFormat)
	}
	writer := estargz.NewWriterWithCompressor(compressed, contentHashingCompressor{Compressor: compressor, contentHash: contentHash})
	return &EstargzWriter{writer: writer, compressionFormat: compressionFormat}, nil
}

// AppendTar appends a tar entry using estargz Writer.AppendTar
func (e *EstargzWriter) AppendTar(r io.Reader) error {
	return e.writer.AppendTar(r)
}

// Close closes the estargz writer
func (e *EstargzWriter) Close() error {
	digest, err := e.writer.Close()
	if err != nil {
		return err
	}
	e.tocDigest = digest.String()
	return nil
}

func (e *EstargzWriter) Annotations() map[string]string {
	return map[string]string{api.TocDigestAnnotation: e.tocDigest}
}

// EstargzGzipCompressorMaker implements tarCompressorMaker for EstargzWriter with gzip
type EstargzGzipCompressorMaker struct{}

func (EstargzGzipCompressorMaker) NewWriter(compressed io.Writer, contentHash CountingResumableHash) *EstargzWriter {
	return NewEstargzWriter(compressed, contentHash)
}

func (EstargzGzipCompressorMaker) NewWriterLevel(compressed io.Writer, contentHash CountingResumableHash, level int) (*EstargzWriter, error) {
	return NewEstargzWriterLevel(compressed, contentHash, level)
}

func (EstargzGzipCompressorMaker) Name() string {
	return "gzip"
}

// EstargzZstdCompressorMaker implements tarCompressorMaker for EstargzWriter with zstd
type EstargzZstdCompressorMaker struct{}

func (EstargzZstdCompressorMaker) NewWriter(compressed io.Writer, contentHash CountingResumableHash) *EstargzWriter {
	writer, _ := NewEstargzWriterWithCompression(compressed, contentHash, "zstd", 3) // default level
	return writer
}

func (EstargzZstdCompressorMaker) NewWriterLevel(compressed io.Writer, contentHash CountingResumableHash, level int) (*EstargzWriter, error) {
	return NewEstargzWriterWithCompression(compressed, contentHash, "zstd", level)
}

func (EstargzZstdCompressorMaker) Name() string {
	return "zstd"
}
