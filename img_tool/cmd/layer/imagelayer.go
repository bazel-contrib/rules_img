package layer

import (
	"archive/tar"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/metadata"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/merkle"
)

// imageBuilder is what the image and archive writers (ext4, FAT32 and cpio) share with
// the tar writer: tar headers, with content from a reader, a path or an entry
// of a tar archive that is read when the image is written.
type imageBuilder interface {
	WriteHeader(hdr *tar.Header) error
	WriteRegular(hdr *tar.Header, r io.Reader) error
	WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error
	WriteRegularDeduplicatedKnownHash(hdr *tar.Header, r io.Reader, blobHash []byte) error
	WriteRegularFromPath(hdr *tar.Header, filePath string) error
	WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error
	WriteRegularFromTarEntry(hdr *tar.Header, archive tree.TarArchive, index int, knownHash []byte) error
}

// imageSink adapts a filesystem image or archive builder to the sink the layer
// recorder writes into, so that every input of "img layer" can be written as an
// image. Like in tar layers, regular files with the same content and metadata
// are stored once, as hard links of each other (if the builder was asked to).
// The content-addressed state shared between layers (--deduplicate) does not
// apply to images.
type imageSink struct {
	builder  imageBuilder
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
	_ api.TarCAS        = (*imageSink)(nil)
	_ tree.TarEntrySink = (*imageSink)(nil)
)

func (s *imageSink) Close() error { return nil }

func (s *imageSink) WriteHeader(hdr *tar.Header) error { return s.builder.WriteHeader(hdr) }

func (s *imageSink) WriteRegular(hdr *tar.Header, r io.Reader) error {
	return s.builder.WriteRegular(hdr, r)
}

func (s *imageSink) WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error {
	return s.builder.WriteRegularDeduplicated(hdr, r)
}

func (s *imageSink) WriteRegularDeduplicatedKnownHash(hdr *tar.Header, r io.Reader, blobHash []byte) error {
	return s.builder.WriteRegularDeduplicatedKnownHash(hdr, r, blobHash)
}

func (s *imageSink) WriteRegularFromPath(hdr *tar.Header, filePath string) error {
	return s.builder.WriteRegularFromPath(hdr, filePath)
}

// WriteRegularFromTarEntry lets the builder read an imported tar entry when the
// image is written, so that the content does not have to be kept until then.
func (s *imageSink) WriteRegularFromTarEntry(hdr *tar.Header, archive tree.TarArchive, index int, knownHash []byte) error {
	return s.builder.WriteRegularFromTarEntry(hdr, archive, index, knownHash)
}

func (s *imageSink) WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error {
	return s.builder.WriteRegularFromPathDeduplicated(hdr, filePath)
}

func (s *imageSink) Import(api.CASStateSupplier) error { return nil }
func (s *imageSink) Export(api.CASStateExporter) error { return nil }

// ContentHasher is used by the recorder to digest tar entries before writing
// them. The builders take SHA-256 digests.
func (s *imageSink) ContentHasher() hash.Hash { return sha256.New() }

var errImageNoCAS = errors.New("content-addressed storage is not supported for image layers")

func (s *imageSink) Store(io.Reader, string) (string, []byte, int64, error) {
	return "", nil, 0, errImageNoCAS
}

func (s *imageSink) StoreKnownHashAndSize(io.Reader, []byte, int64, string) (string, error) {
	return "", errImageNoCAS
}

func (s *imageSink) StoreNode(io.Reader, *tar.Header) (string, []byte, int64, error) {
	return "", nil, 0, errImageNoCAS
}

func (s *imageSink) StoreNodeKnownHash(io.Reader, *tar.Header, []byte) (string, error) {
	return "", errImageNoCAS
}

func (s *imageSink) StoreTreeKnownHash(fsys fs.FS, intendedPath string, treeHash []byte) (string, error) {
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
func (s *imageSink) StoreTree(fsys fs.FS, intendedPath string) (string, error) {
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
func (s *imageSink) deduplicatesTrees() bool {
	if !s.deduplicateTrees {
		return false
	}
	return s.metadata == nil || (s.metadata.Defaults == nil && len(s.metadata.FileOverrides) == 0)
}

// storeTree writes the tree below intendedPath. With treeHash set, a tree with
// the same hash that was stored before is reused: its path is returned and
// nothing is written.
func (s *imageSink) storeTree(fsys fs.FS, intendedPath string, treeHash []byte) (string, error) {
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

func (s *imageSink) applyMetadata(hdr *tar.Header) error {
	if s.metadata == nil {
		return nil
	}
	return s.metadata.applyToGeneratedHeader(hdr)
}

// tempImage creates a temporary file for an image that only exists to be turned
// into the compact stream. It is next to the compact stream, which is where a
// build action may write.
func tempImage(compactStreamPath, pattern string) (*os.File, func(), error) {
	tmp, err := os.CreateTemp(filepath.Dir(compactStreamPath), pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("creating temporary image: %w", err)
	}
	return tmp, func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}, nil
}

// imageLayer describes the file a layer was written as.
type imageLayer struct {
	mediaType string
	// digest is the digest of the layer file, and diffID the digest of its
	// content once decompressed. They only differ for compressed layers.
	digest, diffID string
	size           int64
}

// handleImageLayer writes the layer as a filesystem image or archive and, if
// requested, the layer metadata describing it. With a nil image
// (compact-stream-only), the image only exists temporarily, to be turned into
// the compact stream.
func handleImageLayer(
	format api.ContainerFormat, ext4Settings ext4Settings, fat32Settings fat32Settings, cpioSettings cpioSettings, image *os.File, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
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
	if image == nil && compactStream == nil {
		return errors.New("no image and no compact stream to write")
	}
	var layer imageLayer
	switch format {
	case api.Ext4Format:
		if image == nil {
			var cleanup func()
			if image, cleanup, err = tempImage(compactStreamPath, "ext4-layer-*"); err != nil {
				return err
			}
			defer cleanup()
		}
		info, err := writeExt4Layer(image, ext4Settings, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata, compactStream, compactStreamInlineThreshold)
		if err != nil {
			return err
		}
		digest := fmt.Sprintf("sha256:%x", info.Digest)
		layer = imageLayer{mediaType: api.Ext4Layer, digest: digest, diffID: digest, size: info.Size}
	case api.Fat32Format:
		if image == nil {
			var cleanup func()
			if image, cleanup, err = tempImage(compactStreamPath, "fat32-layer-*"); err != nil {
				return err
			}
			defer cleanup()
		}
		info, err := writeFat32Layer(image, fat32Settings, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata, compactStream, compactStreamInlineThreshold)
		if err != nil {
			return err
		}
		digest := fmt.Sprintf("sha256:%x", info.Digest)
		layer = imageLayer{mediaType: api.Fat32Layer, digest: digest, diffID: digest, size: info.Size}
	case api.CpioFormat:
		layer, err = writeCpioLayer(image, cpioSettings, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata, compactStream, compactStreamInlineThreshold, compactStreamPath)
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
	if err := writeImageMetadata(layer, history, mediaType, annotations, metadataFile); err != nil {
		_ = metadataFile.Close() // the write error is the one worth reporting
		return err
	}
	return metadataFile.Close()
}

// writeImageMetadata writes the layer metadata file for an image layer. The
// digest and size describe the layer file; other than that, the metadata is
// only as faithful as the layer rules need it to be, since image layers are not
// OCI layers.
func writeImageMetadata(layer imageLayer, history, mediaTypeOverride string, annotations map[string]string, output io.Writer) error {
	var layerHistory []api.History
	if history == "" {
		layerHistory = []api.History{}
	} else {
		layerHistory = api.LayerHistory(history)
	}
	mediaType := layer.mediaType
	if mediaTypeOverride != "" {
		mediaType = mediaTypeOverride
	}
	return metadata.WriteLayerMetadata(layer.diffID, mediaType, layer.digest, layer.size, annotations, layerHistory, output)
}
