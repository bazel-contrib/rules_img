package ztoc

import (
	"fmt"
	"io"
	"os"
)

// config holds the resolved build options.
type config struct {
	spanSize            int64
	buildToolIdentifier string
}

func defaultConfig() config {
	return config{
		spanSize:            DefaultSpanSize,
		buildToolIdentifier: DefaultBuildToolIdentifier,
	}
}

// Option customizes ztoc construction.
type Option func(*config)

// WithSpanSize sets the minimum number of uncompressed bytes between
// checkpoints. It must be positive; the default is [DefaultSpanSize] (4 MiB).
func WithSpanSize(bytes int64) Option {
	return func(c *config) { c.spanSize = bytes }
}

// WithBuildToolIdentifier sets the ztoc's build_tool_identifier field.
func WithBuildToolIdentifier(id string) Option {
	return func(c *config) { c.buildToolIdentifier = id }
}

// Build constructs a ztoc for a gzip-compressed tar layer. r must provide the
// compressed blob and size must be its exact length in bytes.
//
// Build currently reads the whole compressed blob into memory.
func Build(r io.ReaderAt, size int64, opts ...Option) (*Ztoc, error) {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.spanSize <= 0 {
		return nil, fmt.Errorf("ztoc: span size must be positive, got %d", cfg.spanSize)
	}
	if size < 0 {
		return nil, fmt.Errorf("ztoc: negative size %d", size)
	}
	if int64(int(size)) != size {
		return nil, fmt.Errorf("ztoc: size %d too large for this platform", size)
	}

	data := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(r, 0, size), data); err != nil {
		return nil, fmt.Errorf("ztoc: reading compressed blob: %w", err)
	}

	z, err := buildGzipZinfo(data, cfg.spanSize)
	if err != nil {
		return nil, err
	}
	digests, err := z.spanDigests(data, size)
	if err != nil {
		return nil, err
	}
	toc, uncompressedSize, err := buildTOC(data)
	if err != nil {
		return nil, err
	}

	return &Ztoc{
		Version:                 Version09,
		BuildToolIdentifier:     cfg.buildToolIdentifier,
		CompressedArchiveSize:   Offset(size),
		UncompressedArchiveSize: uncompressedSize,
		TOC:                     toc,
		CompressionInfo: CompressionInfo{
			MaxSpanID:            z.maxSpanID(),
			SpanDigests:          digests,
			Checkpoints:          z.marshalCheckpoints(),
			CompressionAlgorithm: CompressionGzip,
		},
	}, nil
}

// BuildFromFile constructs a ztoc for the gzip-compressed tar layer at path.
func BuildFromFile(path string, opts ...Option) (*Ztoc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return Build(f, fi.Size(), opts...)
}
