package layer

import (
	"archive/tar"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4/ext4cstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
)

// ext4Image lets the layer sink drive an ext4 builder.
type ext4Image struct{ *ext4.Builder }

func (b ext4Image) WriteRegularFromTarEntry(hdr *tar.Header, archive tree.TarArchive, index int, knownHash []byte) error {
	return b.Builder.WriteRegularFromTarEntry(hdr, archive, index, knownHash)
}

// ext4Settings are the ext4-only knobs of a layer action (--ext4-*).
type ext4Settings struct {
	blockSize        int
	size             int64
	bytesPerInode    int64
	extraInodes      uint32
	label            string
	uuid             [16]byte
	omitLostAndFound bool
	deduplicate      bool
	// deduplicateTrees comes from --layer-tree-artifact-handling, which is not
	// ext4-specific.
	deduplicateTrees bool
}

// options turns the settings into the options of the ext4 writer. The writer
// validates them.
func (s ext4Settings) options() ext4.Options {
	return ext4.Options{
		BlockSize:        s.blockSize,
		Size:             s.size,
		BytesPerInode:    s.bytesPerInode,
		ExtraInodes:      s.extraInodes,
		Label:            s.label,
		UUID:             s.uuid,
		OmitLostAndFound: s.omitLostAndFound,
		Deduplicate:      s.deduplicate,
	}
}

// parseUUID parses the textual form of a UUID, with or without its hyphens.
// The nil UUID is rejected, because it is what asks the writer to derive one.
func parseUUID(raw string) ([16]byte, error) {
	var uuid [16]byte
	digits := strings.ReplaceAll(raw, "-", "")
	if len(digits) != 2*len(uuid) {
		return uuid, fmt.Errorf("%q is not a UUID: want %d hexadecimal digits, got %d", raw, 2*len(uuid), len(digits))
	}
	if _, err := hex.Decode(uuid[:], []byte(digits)); err != nil {
		return uuid, fmt.Errorf("%q is not a UUID: %w", raw, err)
	}
	if uuid == ([16]byte{}) {
		return uuid, fmt.Errorf("the nil UUID cannot be requested; leave the UUID unset to derive it from the content")
	}
	return uuid, nil
}

// writeExt4Layer writes the layer as an ext4 image to out, which must be a new,
// empty file. If compactStream is set, a compact stream of the image is
// written to it. Options that only apply to tar layers (compression, estargz,
// ztoc) have no effect.
func writeExt4Layer(
	out *os.File, settings ext4Settings, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
	compactStream io.Writer, compactStreamInlineThreshold uint64,
) (*ext4.Info, error) {
	opts := settings.options()
	// The digest of the image is needed for the metadata, and is computed
	// while the image is written.
	opts.DigestImage = true
	opts.DigestFiles = compactStream != nil
	builder, err := ext4.NewBuilder(opts)
	if err != nil {
		return nil, err
	}
	defer builder.Close()

	sink := &imageSink{builder: ext4Image{builder}, metadata: layerMetadata, deduplicateTrees: settings.deduplicateTrees}
	recorder := tree.NewRecorder(sink).WithDeduplicate(settings.deduplicate)
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
