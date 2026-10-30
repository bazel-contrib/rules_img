package compress

import "runtime"

type Option interface {
	apply(*options)
}

type ContentType string

type HashAlgorithm string

type CompressionAlgorithm string

type CompressionLevel int

type options struct {
	contentType      ContentType
	compressionLevel *CompressionLevel
	compressorJobs   *int
	zstdWindowLog    *int
}

func (c ContentType) apply(opts *options)      { opts.contentType = c }
func (l CompressionLevel) apply(opts *options) { opts.compressionLevel = &l }

type CompressorJobs int

func (j CompressorJobs) apply(opts *options) { v := int(j); opts.compressorJobs = &v }

// ZstdWindowLog sets the zstd encoder's window size to 2^n bytes, like the zstd
// CLI's --long=n. It only affects plain zstd layers; gzip, estargz and
// zstd:chunked ignore it.
type ZstdWindowLog int

func (w ZstdWindowLog) apply(opts *options) { v := int(w); opts.zstdWindowLog = &v }

// jobs returns the requested number of compressor jobs, resolving the "all
// cores" convention (any negative value) to a concrete count, or 0 when the
// option is unset.
func (o options) jobs() int {
	if o.compressorJobs == nil {
		return 0
	}
	if *o.compressorJobs < 0 {
		return runtime.NumCPU()
	}
	return *o.compressorJobs
}
