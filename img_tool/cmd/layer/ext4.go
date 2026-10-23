package layer

import (
	"io"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4/ext4cstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
)

// writeExt4Layer writes the layer as an ext4 image to out, which must be a new,
// empty file. If compactStream is set, a compact stream of the image is
// written to it. Options that only apply to tar layers (compression, estargz,
// ztoc) have no effect.
func writeExt4Layer(
	out *os.File, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
	compactStream io.Writer, compactStreamInlineThreshold uint64,
) (*ext4.Info, error) {
	builder, err := ext4.NewBuilder(ext4.Options{Deduplicate: true, DigestFiles: compactStream != nil})
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
	return ext4cstream.Build(builder, out, compactStream, ext4cstream.Options{MinSize: max(int64(compactStreamInlineThreshold), 1)})
}
