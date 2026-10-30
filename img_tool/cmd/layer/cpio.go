package layer

import (
	"archive/tar"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio/cpiocstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
)

// cpioImage lets the layer sink drive a cpio builder.
type cpioImage struct{ *cpio.Builder }

func (b cpioImage) WriteRegularFromTarEntry(hdr *tar.Header, archive tree.TarArchive, index int, knownHash []byte) error {
	return b.Builder.WriteRegularFromTarEntry(hdr, archive, index, knownHash)
}

// cpioSettings are the cpio-only knobs of a layer action (--cpio-*), and how
// the archive is compressed. Unlike a filesystem image, a cpio archive is
// usually compressed (the kernel unpacks gzip and zstd as they are), so the
// compression flags of the command apply.
type cpioSettings struct {
	deduplicate bool
	// deduplicateTrees comes from --layer-tree-artifact-handling, which is not
	// cpio-specific.
	deduplicateTrees bool

	compression      api.CompressionAlgorithm
	compressorJobs   string
	compressionLevel int
	zstdWindowLog    int
}

// mediaType is the media type recorded for the layer file.
func (s cpioSettings) mediaType() string {
	switch s.compression {
	case api.Gzip:
		return api.CpioGzipLayer
	case api.Zstd:
		return api.CpioZstdLayer
	}
	return api.CpioLayer
}

// writeCpioLayer writes the layer as a cpio archive in the newc format (an
// initramfs) to out, which must be a new, empty file opened for reading and
// writing, or nil if only the compact stream is wanted. The archive is
// compressed as asked. If compactStream is set, a compact stream of the archive
// is written to it. The options that only apply to tar layers (estargz, ztoc)
// have no effect.
func writeCpioLayer(
	out *os.File, settings cpioSettings, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
	compactStream io.Writer, compactStreamInlineThreshold uint64, compactStreamPath string,
) (imageLayer, error) {
	var layer imageLayer
	builder, err := cpio.NewBuilder(cpio.Options{Deduplicate: settings.deduplicate, DigestFiles: compactStream != nil})
	if err != nil {
		return layer, err
	}
	defer builder.Close()

	sink := &imageSink{builder: cpioImage{builder}, metadata: layerMetadata, deduplicateTrees: settings.deduplicateTrees}
	recorder := tree.NewRecorder(sink).WithDeduplicate(settings.deduplicate)
	if layerMetadata != nil {
		recorder = recorder.WithMetadata(layerMetadata)
	}
	if err := writeLayer(recorder, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata); err != nil {
		return layer, err
	}
	if layerMetadata != nil {
		if err := layerMetadata.VerifyAllFileMetadataUsed(); err != nil {
			return layer, err
		}
	}

	var csLevel int8
	if compactStream != nil {
		if csLevel, err = compactStreamCompressionLevel(settings.compressionLevel); err != nil {
			return layer, err
		}
	}

	var layerFile io.Writer = io.Discard
	if out != nil {
		layerFile = out
	}
	appender, err := compress.AppenderFactory("sha256", string(settings.compression), layerFile, compressOptions(settings.compressionLevel, settings.compressorJobs, settings.zstdWindowLog)...)
	if err != nil {
		return layer, fmt.Errorf("creating compressor: %w", err)
	}

	// The compact stream describes the uncompressed archive, so the archive
	// has to be read back after it was written. With no compression, that is
	// the layer file. Otherwise the archive is also written to a temporary
	// file, next to the compact stream, which is where a build action may
	// write.
	var plain io.ReaderAt
	var archive io.Writer = appender
	if compactStream != nil {
		if settings.compression == api.Uncompressed && out != nil {
			plain = out
		} else {
			tmp, cleanup, err := tempImage(compactStreamPath, "cpio-archive-*")
			if err != nil {
				return layer, err
			}
			defer cleanup()
			plain, archive = tmp, io.MultiWriter(appender, tmp)
		}
	}
	info, err := builder.Build(archive)
	if err != nil {
		return layer, err
	}
	state, err := appender.Finalize()
	if err != nil {
		return layer, fmt.Errorf("finalizing compressor: %w", err)
	}
	layer = imageLayer{
		mediaType: settings.mediaType(),
		digest:    fmt.Sprintf("sha256:%x", state.OuterHash),
		diffID:    fmt.Sprintf("sha256:%x", state.ContentHash),
		size:      state.CompressedSize,
	}
	if compactStream == nil {
		return layer, nil
	}

	var original compactstream.OriginalCompressionInfo
	switch settings.compression {
	case api.Gzip:
		original.Compression = compactstream.OriginalCompressionGzip
	case api.Zstd:
		original.Compression = compactstream.OriginalCompressionZstd
		original.ZstdWindowLog = uint8(settings.zstdWindowLog)
	}
	if original.Compression != compactstream.OriginalCompressionNone {
		original.CompressionLevel = csLevel
		original.CompressorJobs = recordedCompressorJobs(settings.compressorJobs)
	}
	// The compressor already hashed the archive.
	info.Digest = state.ContentHash
	// Like for tar layers, files smaller than the inline threshold stay in the
	// compact stream. Zero disables inlining.
	err = cpiocstream.Write(info, plain, compactStream, cpiocstream.Options{
		MinSize:          int64(min(max(compactStreamInlineThreshold, 1), math.MaxInt64)),
		Original:         original,
		CompressedDigest: state.OuterHash,
		CompressedSize:   uint64(state.CompressedSize),
	})
	return layer, err
}
