package layer

import (
	"io"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32/fat32cstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
)

// writeFat32Layer writes the layer as a FAT32 image to out, which must be a new,
// empty file. If compactStream is set, a compact stream of the image is
// written to it. Options that only apply to tar layers (compression, estargz,
// ztoc) have no effect. FAT cannot store symbolic links, device nodes and
// FIFOs, so layers with such entries fail.
func writeFat32Layer(
	out *os.File, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
	compactStream io.Writer, compactStreamInlineThreshold uint64,
) (*fat32.Info, error) {
	builder, err := fat32.NewBuilder(fat32.Options{DigestFiles: compactStream != nil})
	if err != nil {
		return nil, err
	}
	defer builder.Close()

	recorder := tree.NewRecorder(&imageSink{builder: builder, metadata: layerMetadata})
	if layerMetadata != nil {
		recorder = recorder.WithMetadata(layerMetadata)
	}
	if err := writeLayer(recorder, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata); err != nil {
		return nil, err
	}
	if layerMetadata != nil {
		if err := layerMetadata.VerifyAllFileMetadataUsed(); err != nil {
			return nil, err
		}
	}
	if compactStream == nil {
		return builder.Build(out)
	}
	// Like for tar layers, files smaller than the inline threshold stay in the
	// compact stream. Zero disables inlining.
	return fat32cstream.Build(builder, out, compactStream, fat32cstream.Options{MinSize: max(int64(compactStreamInlineThreshold), 1)})
}
