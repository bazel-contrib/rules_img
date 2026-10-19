package compress

import (
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"io"

	"github.com/klauspost/compress/zstd"
	pgzip "github.com/klauspost/pgzip"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

type SHA256Maker struct{}

func (SHA256Maker) New() ResumableHash {
	h := sha256.New()
	return h.(ResumableHash)
}

func (SHA256Maker) Name() string {
	return "sha256"
}

type GZipMaker struct{}

func (GZipMaker) NewWriter(w io.Writer) *gzip.Writer {
	return gzip.NewWriter(w)
}

func (GZipMaker) NewWriterLevel(w io.Writer, level int) (*gzip.Writer, error) {
	return gzip.NewWriterLevel(w, level)
}

func (GZipMaker) Name() string {
	return "gzip"
}

// PGZipMaker uses klauspost/pgzip (parallel gzip)
type PGZipMaker struct{}

func (PGZipMaker) NewWriter(w io.Writer) *pgzip.Writer {
	// default pgzip writer; caller may adjust concurrency via options in pipeline
	return pgzip.NewWriter(w)
}

func (PGZipMaker) NewWriterLevel(w io.Writer, level int) (*pgzip.Writer, error) {
	return pgzip.NewWriterLevel(w, level)
}

func (PGZipMaker) Name() string { return "gzip" }

type nopCompressor struct {
	underlying io.Writer
}

func (n nopCompressor) Close() error {
	return nil
}

func (n nopCompressor) Flush() error {
	return nil
}

func (n nopCompressor) Write(p []byte) (int, error) {
	return n.underlying.Write(p)
}

type UncompressedMaker struct{}

func (UncompressedMaker) NewWriter(w io.Writer) nopCompressor {
	return nopCompressor{underlying: w}
}

func (UncompressedMaker) NewWriterLevel(w io.Writer, level int) (nopCompressor, error) {
	return nopCompressor{underlying: w}, nil
}

func (UncompressedMaker) Name() string {
	return "uncompressed"
}

type ZstdMaker struct{}

// zstdMinEncoderConcurrency is the floor for the zstd encoder's worker count,
// and the default when no job count is requested.
//
// zstd silently falls back to sequential stream encoding when concurrency is
// <= 1, which emits different bytes than the parallel path. Keeping a floor of
// 2 makes --compressor-jobs 1, and a single-core machine, produce the same
// layer as every other configuration.
const zstdMinEncoderConcurrency = 2

// zstdEncoderConcurrency is the number of goroutines the zstd encoder may use
// for one stream.
//
// It follows --compressor-jobs, which is the knob users already have to trade
// compression speed against resource use: a job count is used as requested,
// "nproc" takes every available core, and an unset job count stays at the floor
// the same way gzip stays single-threaded when nothing asks for more. Each
// worker costs roughly 64 MiB of peak memory for its job buffers, so asking for
// more cores than the input has jobs only costs memory.
func zstdEncoderConcurrency(opts options) int {
	return max(opts.jobs(), zstdMinEncoderConcurrency)
}

// zstdEncoderOptions returns the encoder options for a zstd layer.
//
// By default klauspost/compress uses "light" concurrency for stream encoding:
// at most two goroutines work on a stream, so a large layer compresses on about
// two cores no matter how many the machine has. WithConcurrentBlocks switches
// the encoder to job-based parallelism, which splits the input into fixed-size
// jobs, compresses them concurrently and writes the results in order.
//
// The output does not depend on the worker count: the job size, the overlap
// prefix each job gets for match context, and the block size are all derived
// from the encoder level and window size alone, and results are flushed in job
// order. The only worker count that changes the bytes is 1, which disables the
// parallel path entirely - see zstdMinEncoderConcurrency.
func zstdEncoderOptions(opts options) []zstd.EOption {
	encoderOptions := []zstd.EOption{
		zstd.WithConcurrentBlocks(true),
		zstd.WithEncoderConcurrency(zstdEncoderConcurrency(opts)),
	}
	if opts.compressionLevel != nil {
		encoderOptions = append(encoderOptions, zstd.WithEncoderLevel(zstd.EncoderLevel(*opts.compressionLevel)))
	}
	return encoderOptions
}

// NewWriterOptions implements configurableCompressorMaker: unlike pgzip, the
// zstd encoder's concurrency has to be chosen when the encoder is created, so
// the compression level and the job count are applied together here. It is the
// only place that builds a zstd encoder.
func (ZstdMaker) NewWriterOptions(w io.Writer, opts options) (*zstd.Encoder, error) {
	return zstd.NewWriter(w, zstdEncoderOptions(opts)...)
}

func (m ZstdMaker) NewWriter(w io.Writer) *zstd.Encoder {
	encoder, _ := m.NewWriterOptions(w, options{})
	return encoder
}

func (m ZstdMaker) NewWriterLevel(w io.Writer, level int) (*zstd.Encoder, error) {
	compressionLevel := CompressionLevel(level)
	return m.NewWriterOptions(w, options{compressionLevel: &compressionLevel})
}

func (ZstdMaker) Name() string {
	return "zstd"
}

func NewSHA256GzipAppender(w io.Writer, options ...Option) (Appender[*gzip.Writer], error) {
	return New[*gzip.Writer, SHA256Maker, GZipMaker](w, options...)
}

func NewSHA256PGzipAppender(w io.Writer, options ...Option) (Appender[*pgzip.Writer], error) {
	return New[*pgzip.Writer, SHA256Maker, PGZipMaker](w, options...)
}

func ResumeSHA256GzipAppender(state api.AppenderState, w io.Writer, options ...Option) (Appender[*gzip.Writer], error) {
	return Resume[*gzip.Writer, SHA256Maker, GZipMaker](state, w, options...)
}

func ResumeSHA256PGzipAppender(state api.AppenderState, w io.Writer, options ...Option) (Appender[*pgzip.Writer], error) {
	return Resume[*pgzip.Writer, SHA256Maker, PGZipMaker](state, w, options...)
}

func NewSHA256ZstdAppender(w io.Writer, options ...Option) (Appender[*zstd.Encoder], error) {
	return New[*zstd.Encoder, SHA256Maker, ZstdMaker](w, options...)
}

func ResumeSHA256ZstdAppender(state api.AppenderState, w io.Writer, options ...Option) (Appender[*zstd.Encoder], error) {
	return Resume[*zstd.Encoder, SHA256Maker, ZstdMaker](state, w, options...)
}

func NewSHA256EstargzGzipTarAppender(w io.Writer, options ...Option) (*TarAppender[*EstargzWriter], error) {
	appender, err := NewTar[*EstargzWriter, SHA256Maker, EstargzGzipCompressorMaker](w, options...)
	if err != nil {
		return nil, err
	}
	return &appender, nil
}

func ResumeSHA256EstargzGzipTarAppender(state api.AppenderState, w io.Writer, options ...Option) (*TarAppender[*EstargzWriter], error) {
	appender, err := ResumeTar[*EstargzWriter, SHA256Maker, EstargzGzipCompressorMaker](state, w, options...)
	if err != nil {
		return nil, err
	}
	return &appender, nil
}

func NewSHA256EstargzZstdTarAppender(w io.Writer, options ...Option) (*TarAppender[*EstargzWriter], error) {
	appender, err := NewTar[*EstargzWriter, SHA256Maker, EstargzZstdCompressorMaker](w, options...)
	if err != nil {
		return nil, err
	}
	return &appender, nil
}

func ResumeSHA256EstargzZstdTarAppender(state api.AppenderState, w io.Writer, options ...Option) (*TarAppender[*EstargzWriter], error) {
	appender, err := ResumeTar[*EstargzWriter, SHA256Maker, EstargzZstdCompressorMaker](state, w, options...)
	if err != nil {
		return nil, err
	}
	return &appender, nil
}

func collectOptions(optionsList ...Option) options {
	opts := options{}
	for _, o := range optionsList {
		o.apply(&opts)
	}
	return opts
}

func AppenderFactory(hashAlgorithm, compressionAlgorithm string, w io.Writer, optionsList ...Option) (api.Appender, error) {
	opts := collectOptions(optionsList...)
	switch {
	case hashAlgorithm == "sha256" && compressionAlgorithm == "gzip":
		// Use pgzip if jobs > 1 or == -1 (auto) and CPUs > 1
		usePGzip := opts.jobs() > 1
		if usePGzip {
			return NewSHA256PGzipAppender(w, optionsList...)
		}
		return NewSHA256GzipAppender(w, optionsList...)
	case hashAlgorithm == "sha256" && compressionAlgorithm == "zstd":
		return NewSHA256ZstdAppender(w, optionsList...)
	case hashAlgorithm == "sha256" && (compressionAlgorithm == "uncompressed" || compressionAlgorithm == "none"):
		return New[nopCompressor, SHA256Maker, UncompressedMaker](w, optionsList...)
	}
	return nil, errors.New("unsupported hash or compression algorithm")
}

func ResumeFactory(hashAlgorithm, compressionAlgorithm string, state api.AppenderState, w io.Writer, optionsList ...Option) (api.Appender, error) {
	opts := collectOptions(optionsList...)
	switch {
	case hashAlgorithm == "sha256" && compressionAlgorithm == "gzip":
		usePGzip := opts.jobs() > 1
		if usePGzip {
			return ResumeSHA256PGzipAppender(state, w, optionsList...)
		}
		return ResumeSHA256GzipAppender(state, w, optionsList...)
	case hashAlgorithm == "sha256" && compressionAlgorithm == "zstd":
		return ResumeSHA256ZstdAppender(state, w, optionsList...)
	case hashAlgorithm == "sha256" && (compressionAlgorithm == "uncompressed" || compressionAlgorithm == "none"):
		return Resume[nopCompressor, SHA256Maker, UncompressedMaker](state, w, optionsList...)
	}
	return nil, errors.New("unsupported hash or compression algorithm")
}

func TarAppenderFactory(hashAlgorithm, compressionAlgorithm string, seekable bool, w io.Writer, optionsList ...Option) (api.TarAppender, error) {
	opts := collectOptions(optionsList...)
	switch {
	case hashAlgorithm == "sha256" && compressionAlgorithm == "gzip" && seekable:
		// estargz path: cannot (easily) parallelize gzip here
		return NewSHA256EstargzGzipTarAppender(w, optionsList...)
	case hashAlgorithm == "sha256" && compressionAlgorithm == "gzip" && !seekable:
		usePGzip := opts.jobs() > 1
		if usePGzip {
			appender, err := NewSHA256PGzipAppender(w, optionsList...)
			if err != nil {
				return nil, err
			}
			return appender.TarAppender(), nil
		}
		appender, err := NewSHA256GzipAppender(w, optionsList...)
		if err != nil {
			return nil, err
		}
		return appender.TarAppender(), nil
	case hashAlgorithm == "sha256" && compressionAlgorithm == "zstd" && seekable:
		return NewSHA256EstargzZstdTarAppender(w, optionsList...)
	case hashAlgorithm == "sha256" && compressionAlgorithm == "zstd" && !seekable:
		appender, err := NewSHA256ZstdAppender(w, optionsList...)
		if err != nil {
			return nil, err
		}
		return appender.TarAppender(), nil
	case hashAlgorithm == "sha256" && (compressionAlgorithm == "uncompressed" || compressionAlgorithm == "none") && !seekable:
		appender, err := New[nopCompressor, SHA256Maker, UncompressedMaker](w, optionsList...)
		if err != nil {
			return nil, err
		}
		return appender.TarAppender(), nil
	}
	return nil, errors.New("unsupported hash or compression algorithm")
}

func ResumeTarFactory(hashAlgorithm, compressionAlgorithm string, seekable bool, state api.AppenderState, w io.Writer, optionsList ...Option) (api.TarAppender, error) {
	opts := collectOptions(optionsList...)
	switch {
	case hashAlgorithm == "sha256" && compressionAlgorithm == "gzip" && seekable:
		return ResumeSHA256EstargzGzipTarAppender(state, w, optionsList...)
	case hashAlgorithm == "sha256" && compressionAlgorithm == "gzip" && !seekable:
		usePGzip := opts.jobs() > 1
		if usePGzip {
			appender, err := ResumeSHA256PGzipAppender(state, w, optionsList...)
			if err != nil {
				return nil, err
			}
			return appender.TarAppender(), nil
		}
		appender, err := ResumeSHA256GzipAppender(state, w, optionsList...)
		if err != nil {
			return nil, err
		}
		return appender.TarAppender(), nil
	case hashAlgorithm == "sha256" && compressionAlgorithm == "zstd" && seekable:
		return ResumeSHA256EstargzZstdTarAppender(state, w, optionsList...)
	case hashAlgorithm == "sha256" && compressionAlgorithm == "zstd" && !seekable:
		appender, err := ResumeSHA256ZstdAppender(state, w, optionsList...)
		if err != nil {
			return nil, err
		}
		return appender.TarAppender(), nil
	case hashAlgorithm == "sha256" && (compressionAlgorithm == "uncompressed" || compressionAlgorithm == "none") && !seekable:
		appender, err := Resume[nopCompressor, SHA256Maker, UncompressedMaker](state, w, optionsList...)
		if err != nil {
			return nil, err
		}
		return appender.TarAppender(), nil
	}
	return nil, errors.New("unsupported hash or compression algorithm")
}
