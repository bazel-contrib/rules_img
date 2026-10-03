package compress

import (
	"encoding"
	"fmt"
	"hash"
	"io"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	pgzip "github.com/klauspost/pgzip"
)

// Appender appends data to a compressed blob, while
// hashing the compressed and uncompressed data.
// It implements the io.Writer interface.
// It is resumable, meaning that it can be resumed from a previous state.
type Appender[C Compressor] struct {
	hashFunctionName         string
	compressionAlgorithmName string
	outerHash                ResumableHash
	contentHash              ResumableHash
	compressor               C
	// pipelineWriter is the writer that pushes
	// uncompressed data into the compression
	// and hashing pipeline, which also keeps
	// track of the bytes appended.
	pipelineWriter *countingWriter
	outputWriter   *countingWriter
	options
}

// New creates a Appender with initial state.
func New[C Compressor, HM hashMaker, CM compressorMaker[C]](output io.Writer, opts ...Option) (Appender[C], error) {
	options := options{}
	for _, opt := range opts {
		opt.apply(&options)
	}

	var hashMaker HM
	outerHash := hashMaker.New()
	contentHash := hashMaker.New()
	outputWriter := &countingWriter{w: output}
	pipelineWriter, compress, err := setupWriterPipeline[C, CM](outputWriter, outerHash, contentHash, options)
	if err != nil {
		return Appender[C]{}, err
	}

	var compressorMaker CM
	return Appender[C]{
		hashFunctionName:         hashMaker.Name(),
		compressionAlgorithmName: compressorMaker.Name(),
		outerHash:                outerHash,
		contentHash:              contentHash,
		compressor:               compress,
		pipelineWriter:           &countingWriter{w: pipelineWriter},
		outputWriter:             outputWriter,
		options:                  options,
	}, nil
}

// Resume resumes the Appender from a previous state.
func Resume[C Compressor, HM hashMaker, CM compressorMaker[C]](state api.AppenderState, output io.Writer, opts ...Option) (Appender[C], error) {
	options := options{}
	for _, opt := range opts {
		opt.apply(&options)
	}

	var hashMaker HM
	outerHash := hashMaker.New()
	contentHash := hashMaker.New()
	if err := outerHash.UnmarshalBinary(state.OuterHashState); err != nil {
		return Appender[C]{}, err
	}
	if err := contentHash.UnmarshalBinary(state.ContentHashState); err != nil {
		return Appender[C]{}, err
	}
	outputWriter := &countingWriter{w: output, n: state.CompressedSize}
	pipelineWriter, compress, err := setupWriterPipeline[C, CM](outputWriter, outerHash, contentHash, options)
	if err != nil {
		return Appender[C]{}, err
	}

	var compressorMaker CM
	appender := Appender[C]{
		hashFunctionName:         hashMaker.Name(),
		compressionAlgorithmName: compressorMaker.Name(),
		outerHash:                outerHash,
		contentHash:              contentHash,
		compressor:               compress,
		pipelineWriter:           &countingWriter{w: pipelineWriter, n: state.UncompressedSize},
		outputWriter:             outputWriter,
		options:                  options,
	}

	if state.Magic != appender.magic() {
		return Appender[C]{}, fmt.Errorf("magic mismatch: expected %s, got %s", appender.magic(), state.Magic)
	}

	return appender, nil
}

// Write writes data to the Appender.
func (a Appender[C]) Write(data []byte) (int, error) {
	return a.pipelineWriter.Write(data)
}

// Flush flushes the Appender (by flushing the compressor).
func (a Appender[C]) Flush() error {
	return a.compressor.Flush()
}

// Finalize finalizes the Appender and returns the state.
func (a Appender[C]) Finalize() (api.AppenderState, error) {
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

	state := api.AppenderState{
		Magic:            a.magic(),
		OuterHashState:   outerHashState,
		OuterHash:        a.outerHash.Sum(nil),
		ContentHashState: contentHashState,
		ContentHash:      a.contentHash.Sum(nil),
		CompressedSize:   a.outputWriter.n,
		UncompressedSize: a.pipelineWriter.n,
	}
	return state, nil
}

func (a Appender[C]) TarAppender() api.TarAppender {
	return &tarAppenderAdapter{
		appender: a,
	}
}

func (a Appender[C]) magic() string {
	magic := fmt.Sprintf("imgv1+compressed+%s+%s", a.hashFunctionName, a.compressionAlgorithmName)
	if len(a.contentType) > 0 {
		magic += "+" + string(a.contentType)
	}
	return magic
}

func setupWriterPipeline[C Compressor, CM compressorMaker[C]](output io.Writer, outerHash, contentHash ResumableHash, opts options) (io.Writer, C, error) {
	// Pipeline for writing data:
	// input: uncompressed data
	// write to inputTee
	// inputTee: tee to compressor and contentHash
	// compressor: compresses data and writes to outputTee
	// outputTee: tee to outerHash and output

	outputTee := io.MultiWriter(output, outerHash)
	var compressorMaker CM
	var compress C
	if opts.compressionLevel != nil {
		var err error
		compress, err = compressorMaker.NewWriterLevel(outputTee, int(*opts.compressionLevel))
		if err != nil {
			return nil, compress, err
		}
	} else {
		compress = compressorMaker.NewWriter(outputTee)
	}
	// Configure optional concurrency for compressors that support it (e.g., pgzip)
	switch any(compress).(type) {
	case *pgzip.Writer:
		if opts.compressorJobs != nil {
			jobs := *opts.compressorJobs
			if err := any(compress).(*pgzip.Writer).SetConcurrency(1<<20, jobs); err != nil {
				return nil, compress, err
			}
		}
	}
	inputTee := io.MultiWriter(compress, contentHash)
	return inputTee, compress, nil
}

type ResumableHash interface {
	hash.Hash
	encoding.BinaryMarshaler
	encoding.BinaryUnmarshaler
}

type Compressor interface {
	Close() error
	Flush() error
	Write([]byte) (int, error)
}

type hashMaker interface {
	New() ResumableHash
	Name() string
}

type compressorMaker[T Compressor] interface {
	NewWriter(w io.Writer) T
	NewWriterLevel(w io.Writer, level int) (T, error)
	Name() string
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

type tarAppenderAdapter struct {
	appender api.Appender
	buf      [32 * 1024]byte
}

func (t *tarAppenderAdapter) AppendTar(r io.Reader) error {
	_, err := io.CopyBuffer(t.appender, r, t.buf[:])
	return err
}

func (t *tarAppenderAdapter) Finalize() (api.AppenderState, error) {
	return t.appender.Finalize()
}
