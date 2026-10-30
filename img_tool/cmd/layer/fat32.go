package layer

import (
	"archive/tar"
	"io"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32/fat32cstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
)

// fat32Image lets the layer sink drive a FAT32 builder.
type fat32Image struct{ *fat32.Builder }

func (b fat32Image) WriteRegularFromTarEntry(hdr *tar.Header, archive tree.TarArchive, index int, knownHash []byte) error {
	return b.Builder.WriteRegularFromTarEntry(hdr, archive, index, knownHash)
}

// fat32Settings are the FAT32-only knobs of a layer action (--fat32-*).
type fat32Settings struct {
	// sectorSize is the logical sector size in bytes. Zero is 512.
	sectorSize int
}

// options turns the settings into the options of the FAT32 writer. The writer
// validates them.
func (s fat32Settings) options() fat32.Options {
	return fat32.Options{SectorSize: s.sectorSize}
}

// writeFat32Layer writes the layer as a FAT32 image to out, which must be a new,
// empty file. If compactStream is set, a compact stream of the image is
// written to it. Options that only apply to tar layers (compression, estargz,
// ztoc) have no effect. FAT cannot store symbolic links, device nodes and
// FIFOs, so layers with such entries fail.
func writeFat32Layer(
	out *os.File, settings fat32Settings, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
	compactStream io.Writer, compactStreamInlineThreshold uint64,
) (*fat32.Info, error) {
	opts := settings.options()
	// The digest of the image is needed for the metadata, and is computed
	// while the image is written.
	opts.DigestImage = true
	opts.DigestFiles = compactStream != nil
	builder, err := fat32.NewBuilder(opts)
	if err != nil {
		return nil, err
	}
	defer builder.Close()

	// FAT has no hard links, so identical files are neither merged by the
	// builder nor replaced by symlinks (a tree artifact is copied again).
	sink := &imageSink{builder: fat32Image{builder}, metadata: layerMetadata}
	recorder := tree.NewRecorder(sink).WithDeduplicate(false)
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
