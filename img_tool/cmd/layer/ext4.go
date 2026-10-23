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

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/metadata"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree"
)

// ext4Sink adapts an ext4 builder to the sink the layer recorder writes
// into, so that every input of "img layer" can be written as an ext4 image.
// Deduplication (and with it the content-addressed state) does not apply to
// ext4 images: every file is stored once, contiguously, as written.
type ext4Sink struct {
	builder  *ext4.Builder
	metadata *LayerMetadata
}

var _ api.TarCAS = (*ext4Sink)(nil)

func (s *ext4Sink) Close() error { return nil }

func (s *ext4Sink) WriteHeader(hdr *tar.Header) error { return s.builder.WriteHeader(hdr) }

func (s *ext4Sink) WriteRegular(hdr *tar.Header, r io.Reader) error {
	return s.builder.WriteRegular(hdr, r)
}

func (s *ext4Sink) WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error {
	return s.builder.WriteRegular(hdr, r)
}

func (s *ext4Sink) WriteRegularDeduplicatedKnownHash(hdr *tar.Header, r io.Reader, _ []byte) error {
	return s.builder.WriteRegular(hdr, r)
}

func (s *ext4Sink) WriteRegularFromPath(hdr *tar.Header, filePath string) error {
	return s.builder.WriteRegularFromPath(hdr, filePath)
}

func (s *ext4Sink) WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error {
	return s.builder.WriteRegularFromPath(hdr, filePath)
}

func (s *ext4Sink) Import(api.CASStateSupplier) error { return nil }
func (s *ext4Sink) Export(api.CASStateExporter) error { return nil }

// ContentHasher is used by the recorder to digest tar entries before writing
// them. The digests are not needed here, but the recorder asks for the hasher.
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

func (s *ext4Sink) StoreTreeKnownHash(fsys fs.FS, intendedPath string, _ []byte) (string, error) {
	return s.StoreTree(fsys, intendedPath)
}

// StoreTree writes a directory tree (a tree artifact) below intendedPath. Like
// for tar layers, only regular files are stored, each with mode 0755 unless
// the layer metadata says otherwise. An empty link path is returned to tell the
// recorder that the contents have been written.
func (s *ext4Sink) StoreTree(fsys fs.FS, intendedPath string) (string, error) {
	dir := &tar.Header{Typeflag: tar.TypeDir, Name: intendedPath + "/", Mode: 0o755}
	if err := s.applyMetadata(dir); err != nil {
		return "", err
	}
	if err := s.builder.WriteHeader(dir); err != nil {
		return "", err
	}

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
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
		return s.builder.WriteRegular(hdr, f)
	})
	return "", err
}

func (s *ext4Sink) applyMetadata(hdr *tar.Header) error {
	if s.metadata == nil {
		return nil
	}
	return s.metadata.applyToGeneratedHeader(hdr)
}

// handleImageLayer writes the layer as a filesystem image and, if requested,
// the layer metadata describing it.
func handleImageLayer(
	format api.ContainerFormat, image *os.File, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
	history, mediaType string, annotations map[string]string, metadataPath string,
) error {
	var size int64
	switch format {
	case api.Ext4Format:
		info, err := writeExt4Layer(image, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata)
		if err != nil {
			return err
		}
		size = info.Size
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
	if err := writeExt4Metadata(image, size, history, mediaType, annotations, metadataFile); err != nil {
		metadataFile.Close()
		return err
	}
	return metadataFile.Close()
}

// writeExt4Layer writes the layer as an ext4 image to out, which must be a new,
// empty file. Options that only apply to tar layers (compression, estargz,
// deduplication, compact streams, ztoc) have no effect.
func writeExt4Layer(
	out *os.File, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
) (*ext4.Info, error) {
	builder, err := ext4.NewBuilder(ext4.Options{})
	if err != nil {
		return nil, err
	}
	defer builder.Close()

	recorder := tree.NewRecorder(&ext4Sink{builder: builder, metadata: layerMetadata})
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
	return builder.Build(out)
}

// writeExt4Metadata writes the layer metadata file for an ext4 layer. The
// digest and size describe the image file; other than that, the metadata is
// only as faithful as the layer rules need it to be, since ext4 layers are not
// OCI layers.
func writeExt4Metadata(image *os.File, size int64, history, mediaTypeOverride string, annotations map[string]string, output io.Writer) error {
	hasher := sha256.New()
	if _, err := io.Copy(hasher, io.NewSectionReader(image, 0, size)); err != nil {
		return fmt.Errorf("digesting ext4 image: %w", err)
	}
	digest := fmt.Sprintf("sha256:%x", hasher.Sum(nil))

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
	return metadata.WriteLayerMetadata(digest, mediaType, digest, size, annotations, layerHistory, output)
}
