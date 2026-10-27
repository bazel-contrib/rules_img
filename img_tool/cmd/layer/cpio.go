package layer

import (
	"fmt"
	"io"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio/cpiocstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
)

// cpioCompression is how the cpio archive is compressed to make the layer
// file, with the compressor flags of the command.
type cpioCompression struct {
	algorithm api.CompressionAlgorithm
	jobs      string
	level     int
}

// writeCpioLayer writes the layer as a cpio archive in the newc format (an
// initramfs) to out, which must be a new, empty file, compressed as asked. If
// compactStream is set, a compact stream of the archive is written to it. The
// options that only apply to tar layers (estargz, ztoc) have no effect. It
// returns the digests and sizes of the layer file and of the archive.
func writeCpioLayer(
	out *os.File, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
	compression cpioCompression, compactStream io.Writer, compactStreamInlineThreshold uint64,
) (api.AppenderState, error) {
	var state api.AppenderState
	builder, err := cpio.NewBuilder(cpio.Options{DigestFiles: compactStream != nil})
	if err != nil {
		return state, err
	}
	defer builder.Close()

	recorder := tree.NewRecorder(&imageSink{builder: builder, metadata: layerMetadata})
	if layerMetadata != nil {
		recorder = recorder.WithMetadata(layerMetadata)
	}
	if err := writeLayer(recorder, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata); err != nil {
		return state, err
	}
	if layerMetadata != nil {
		if err := layerMetadata.VerifyAllFileMetadataUsed(); err != nil {
			return state, err
		}
	}

	var csLevel int8
	if compactStream != nil {
		if csLevel, err = compactStreamCompressionLevel(compression.level); err != nil {
			return state, err
		}
	}

	appender, err := compress.AppenderFactory("sha256", string(compression.algorithm), out, compressOptions(compression.level, compression.jobs)...)
	if err != nil {
		return state, fmt.Errorf("creating compressor: %w", err)
	}

	// The compact stream describes the uncompressed archive, so it has to be
	// read back after it was written. With no compression, that is the layer
	// file. Otherwise the archive is also written to a temporary file.
	var plain *os.File
	var archiveWriter io.Writer = appender
	if compactStream != nil {
		if compression.algorithm == api.Uncompressed {
			plain = out
		} else {
			if plain, err = os.CreateTemp("", "cpio-archive-*"); err != nil {
				return state, err
			}
			defer os.Remove(plain.Name())
			defer plain.Close()
			archiveWriter = io.MultiWriter(appender, plain)
		}
	}
	info, err := builder.Build(archiveWriter)
	if err != nil {
		return state, err
	}
	if state, err = appender.Finalize(); err != nil {
		return state, fmt.Errorf("finalizing compressor: %w", err)
	}
	if compactStream == nil {
		return state, nil
	}

	var original compactstream.OriginalCompressionInfo
	switch compression.algorithm {
	case api.Gzip:
		original.Compression = compactstream.OriginalCompressionGzip
	case api.Zstd:
		original.Compression = compactstream.OriginalCompressionZstd
	}
	if original.Compression != compactstream.OriginalCompressionNone {
		original.CompressionLevel = csLevel
		original.CompressorJobs = recordedCompressorJobs(compression.jobs)
	}
	// Like for tar layers, files smaller than the inline threshold stay in the
	// compact stream. Zero disables inlining.
	err = cpiocstream.Write(info, io.NewSectionReader(plain, 0, info.Size), compactStream, cpiocstream.Options{
		MinSize:          max(int64(compactStreamInlineThreshold), 1),
		Original:         original,
		CompressedDigest: state.OuterHash,
		CompressedSize:   uint64(state.CompressedSize),
	})
	return state, err
}
