package compress

import (
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"io"
	"runtime"

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

func (ZstdMaker) NewWriter(w io.Writer) *zstd.Encoder {
	encoder, _ := zstd.NewWriter(w)
	return encoder
}

func (ZstdMaker) NewWriterLevel(w io.Writer, level int) (*zstd.Encoder, error) {
	return zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.EncoderLevel(level)))
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
		usePGzip := false
		if opts.compressorJobs != nil {
			jobs := *opts.compressorJobs
			if jobs < 0 {
				jobs = runtime.NumCPU()
			}
			if jobs > 1 {
				usePGzip = true
			}
		}
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
		usePGzip := false
		if opts.compressorJobs != nil {
			jobs := *opts.compressorJobs
			if jobs < 0 {
				jobs = runtime.NumCPU()
			}
			if jobs > 1 {
				usePGzip = true
			}
		}
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
		usePGzip := false
		if opts.compressorJobs != nil {
			jobs := *opts.compressorJobs
			if jobs < 0 {
				jobs = runtime.NumCPU()
			}
			if jobs > 1 {
				usePGzip = true
			}
		}
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
		usePGzip := false
		if opts.compressorJobs != nil {
			jobs := *opts.compressorJobs
			if jobs < 0 {
				jobs = runtime.NumCPU()
			}
			if jobs > 1 {
				usePGzip = true
			}
		}
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
