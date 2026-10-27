package layer

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4/ext4cstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/metadata"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/merkle"
)

// ext4Sink adapts an ext4 builder to the sink the layer recorder writes
// into, so that every input of "img layer" can be written as an ext4 image.
// Like in tar layers, regular files with the same content and metadata are
// stored once, as hard links of each other. The content-addressed state shared
// between layers (--deduplicate) does not apply to ext4 images.
type ext4Sink struct {
	builder  *ext4.Builder
	metadata *LayerMetadata
	// deduplicateTrees replaces a tree artifact whose content is identical to
	// one that was stored before with a symlink to it
	// (--layer-tree-artifact-handling).
	deduplicateTrees bool
	// firstTreePaths maps the hash of a tree artifact to the path it was first
	// stored at.
	firstTreePaths map[string]string
}

var (
	_ api.TarCAS        = (*ext4Sink)(nil)
	_ tree.TarEntrySink = (*ext4Sink)(nil)
)

func (s *ext4Sink) Close() error { return nil }

func (s *ext4Sink) WriteHeader(hdr *tar.Header) error { return s.builder.WriteHeader(hdr) }

func (s *ext4Sink) WriteRegular(hdr *tar.Header, r io.Reader) error {
	return s.builder.WriteRegular(hdr, r)
}

func (s *ext4Sink) WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error {
	return s.builder.WriteRegularDeduplicated(hdr, r)
}

func (s *ext4Sink) WriteRegularDeduplicatedKnownHash(hdr *tar.Header, r io.Reader, blobHash []byte) error {
	return s.builder.WriteRegularDeduplicatedKnownHash(hdr, r, blobHash)
}

func (s *ext4Sink) WriteRegularFromPath(hdr *tar.Header, filePath string) error {
	return s.builder.WriteRegularFromPath(hdr, filePath)
}

// WriteRegularFromTarEntry lets the builder read an imported tar entry when the
// image is written, so that the content does not have to be kept until then.
func (s *ext4Sink) WriteRegularFromTarEntry(hdr *tar.Header, archive tree.TarArchive, index int, knownHash []byte) error {
	return s.builder.WriteRegularFromTarEntry(hdr, archive, index, knownHash)
}

func (s *ext4Sink) WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error {
	return s.builder.WriteRegularFromPathDeduplicated(hdr, filePath)
}

func (s *ext4Sink) Import(api.CASStateSupplier) error { return nil }
func (s *ext4Sink) Export(api.CASStateExporter) error { return nil }

// ContentHasher is used by the recorder to digest tar entries before writing
// them. The ext4 builder takes SHA-256 digests.
func (s *ext4Sink) ContentHasher() hash.Hash { return sha256.New() }

var errExt4NoCAS = errors.New("content-addressed storage is not supported for ext4 layers")

func (s *ext4Sink) Store(io.Reader, string) (string, []byte, int64, error) {
	return "", nil, 0, errExt4NoCAS
}

func (s *ext4Sink) StoreKnownHashAndSize(io.Reader, []byte, int64, string) (string, error) {
	return "", errExt4NoCAS
}

func (s *ext4Sink) StoreNode(io.Reader, *tar.Header) (string, []byte, int64, error) {
	return "", nil, 0, errExt4NoCAS
}

func (s *ext4Sink) StoreNodeKnownHash(io.Reader, *tar.Header, []byte) (string, error) {
	return "", errExt4NoCAS
}

func (s *ext4Sink) StoreTreeKnownHash(fsys fs.FS, intendedPath string, treeHash []byte) (string, error) {
	if !s.deduplicatesTrees() {
		treeHash = nil
	}
	return s.storeTree(fsys, intendedPath, treeHash)
}

// StoreTree writes a directory tree (a tree artifact) below intendedPath. Like
// for tar layers, only regular files are stored, each with mode 0755 unless
// the layer metadata says otherwise. An empty link path is returned to tell the
// recorder that the contents have been written; with tree deduplication, an
// identical tree that was stored before returns that earlier path instead, and
// the recorder makes this one a symlink to it.
func (s *ext4Sink) StoreTree(fsys fs.FS, intendedPath string) (string, error) {
	if !s.deduplicatesTrees() {
		return s.storeTree(fsys, intendedPath, nil)
	}
	treeHash, err := merkle.NewTreeHasher(fsys, sha256.New).Build()
	if err != nil {
		return "", fmt.Errorf("calculating tree hash before storing tree artifact in layer: %w", err)
	}
	return s.storeTree(fsys, intendedPath, treeHash)
}

// deduplicatesTrees reports whether whole trees may be replaced by symlinks. A
// content-only tree hash cannot represent path-specific metadata, so a layer
// that configures any is not eligible; its files still deduplicate
// individually.
func (s *ext4Sink) deduplicatesTrees() bool {
	if !s.deduplicateTrees {
		return false
	}
	return s.metadata == nil || (s.metadata.Defaults == nil && len(s.metadata.FileOverrides) == 0)
}

// storeTree writes the tree below intendedPath. With treeHash set, a tree with
// the same hash that was stored before is reused: its path is returned and
// nothing is written.
func (s *ext4Sink) storeTree(fsys fs.FS, intendedPath string, treeHash []byte) (string, error) {
	if treeHash != nil {
		if first, ok := s.firstTreePaths[string(treeHash)]; ok {
			return first, nil
		}
	}

	dir := &tar.Header{Typeflag: tar.TypeDir, Name: intendedPath + "/", Mode: 0o755}
	if err := s.applyMetadata(dir); err != nil {
		return "", err
	}
	if err := s.builder.WriteHeader(dir); err != nil {
		return "", err
	}

	if err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking directory %s: %w", p, err)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := fsys.Open(p)
		if err != nil {
			return fmt.Errorf("opening file %s: %w", p, err)
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     path.Join(intendedPath, p),
			Size:     info.Size(),
			Mode:     0o755,
		}
		if err := s.applyMetadata(hdr); err != nil {
			return err
		}
		return s.builder.WriteRegularDeduplicated(hdr, f)
	}); err != nil {
		return "", err
	}

	if treeHash != nil {
		if s.firstTreePaths == nil {
			s.firstTreePaths = map[string]string{}
		}
		s.firstTreePaths[string(treeHash)] = intendedPath
	}
	return "", nil
}

func (s *ext4Sink) applyMetadata(hdr *tar.Header) error {
	if s.metadata == nil {
		return nil
	}
	return s.metadata.applyToGeneratedHeader(hdr)
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

// handleImageLayer writes the layer as a filesystem image and, if requested,
// the layer metadata describing it. With a nil image (compact-stream-only), the
// image only exists temporarily, to be turned into the compact stream.
func handleImageLayer(
	format api.ContainerFormat, ext4Settings ext4Settings, image *os.File, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
	history, mediaType string, annotations map[string]string, metadataPath string,
	compactStreamPath string, compactStreamInlineThreshold uint64,
) (err error) {
	// A nil *os.File in an io.Writer would not be a nil io.Writer.
	var compactStream io.Writer
	if compactStreamPath != "" {
		f, openErr := os.OpenFile(compactStreamPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if openErr != nil {
			return fmt.Errorf("opening compact stream output file: %w", openErr)
		}
		// A write error may only surface when the file is closed, and a
		// truncated compact stream must not pass for a successful output.
		defer func() {
			if closeErr := f.Close(); closeErr != nil && err == nil {
				err = fmt.Errorf("closing compact stream output file: %w", closeErr)
			}
		}()
		compactStream = f
	}
	if image == nil {
		if compactStream == nil {
			return errors.New("no image and no compact stream to write")
		}
		// Next to the compact stream, which is where a build action may write.
		tmp, tmpErr := os.CreateTemp(filepath.Dir(compactStreamPath), "ext4-layer-*")
		if tmpErr != nil {
			return fmt.Errorf("creating temporary image: %w", tmpErr)
		}
		defer func() {
			tmp.Close()
			os.Remove(tmp.Name())
		}()
		image = tmp
	}

	var info *ext4.Info
	switch format {
	case api.Ext4Format:
		info, err = writeExt4Layer(image, ext4Settings, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata, compactStream, compactStreamInlineThreshold)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown image format %q", format)
	}

	if metadataPath == "" {
		return nil
	}
	metadataFile, err := os.OpenFile(metadataPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("opening metadata output file: %w", err)
	}
	if err := writeExt4Metadata(info, history, mediaType, annotations, metadataFile); err != nil {
		metadataFile.Close()
		return err
	}
	return metadataFile.Close()
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

	sink := &ext4Sink{builder: builder, metadata: layerMetadata, deduplicateTrees: settings.deduplicateTrees}
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

// writeExt4Metadata writes the layer metadata file for an ext4 layer. The
// digest and size describe the image file; other than that, the metadata is
// only as faithful as the layer rules need it to be, since ext4 layers are not
// OCI layers.
func writeExt4Metadata(info *ext4.Info, history, mediaTypeOverride string, annotations map[string]string, output io.Writer) error {
	digest := fmt.Sprintf("sha256:%x", info.Digest)

	var layerHistory []api.History
	if history == "" {
		layerHistory = []api.History{}
	} else {
		layerHistory = api.LayerHistory(history)
	}
	mediaType := api.Ext4Layer
	if mediaTypeOverride != "" {
		mediaType = mediaTypeOverride
	}
	return metadata.WriteLayerMetadata(digest, mediaType, digest, info.Size, annotations, layerHistory, output)
}
