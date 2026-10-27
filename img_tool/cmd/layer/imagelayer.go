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
	"github.com/bazel-contrib/rules_img/img_tool/pkg/metadata"
)

// handleImageLayer writes the layer as a filesystem image (ext4 or FAT32) or cpio archive
// and, if requested, the layer metadata describing it.
func handleImageLayer(
	format api.ContainerFormat, image *os.File, addFiles addFiles, importTars importTars, addExecutables executables, addSymlinks symlinks, emptyFiles []string,
	baseMetadataPaths []string, createParentDirectories bool, layerMetadata *LayerMetadata,
	history, mediaType string, annotations map[string]string, metadataPath string,
	compactStreamPath string, compactStreamInlineThreshold uint64, fat32SectorSize int, cpioCompression cpioCompression,
) error {
	// A nil *os.File in an io.Writer would not be a nil io.Writer.
	var compactStream io.Writer
	if compactStreamPath != "" {
		f, err := os.OpenFile(compactStreamPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("opening compact stream output file: %w", err)
		}
		defer f.Close()
		compactStream = f
	}

	var size int64
	var defaultMediaType string
	// state is set for formats that compress, where the digest of the layer file
	// is not the one of the image.
	var state *api.AppenderState
	switch format {
	case api.Ext4Format:
		info, err := writeExt4Layer(image, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata, compactStream, compactStreamInlineThreshold)
		if err != nil {
			return err
		}
		size, defaultMediaType = info.Size, api.Ext4Layer
	case api.CpioFormat:
		s, err := writeCpioLayer(image, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata, cpioCompression, compactStream, compactStreamInlineThreshold)
		if err != nil {
			return err
		}
		state, size = &s, s.CompressedSize
		switch cpioCompression.algorithm {
		case api.Gzip:
			defaultMediaType = api.CpioGzipLayer
		case api.Zstd:
			defaultMediaType = api.CpioZstdLayer
		default:
			defaultMediaType = api.CpioLayer
		}
	case api.Fat32Format:
		info, err := writeFat32Layer(image, addFiles, importTars, addExecutables, addSymlinks, emptyFiles, baseMetadataPaths, createParentDirectories, layerMetadata, compactStream, compactStreamInlineThreshold, fat32SectorSize)
		if err != nil {
			return err
		}
		size, defaultMediaType = info.Size, api.Fat32Layer
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
	if err := writeImageMetadata(image, size, state, history, defaultMediaType, mediaType, annotations, metadataFile); err != nil {
		metadataFile.Close()
		return err
	}
	return metadataFile.Close()
}

// imageBuilder is what the image and archive writers (ext4, FAT32 and cpio) share with
// the tar writer: tar headers, with content from a reader or a path.
type imageBuilder interface {
	WriteHeader(hdr *tar.Header) error
	WriteRegular(hdr *tar.Header, r io.Reader) error
	WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error
	WriteRegularDeduplicatedKnownHash(hdr *tar.Header, r io.Reader, blobHash []byte) error
	WriteRegularFromPath(hdr *tar.Header, filePath string) error
	WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error
}

// imageSink adapts a filesystem image builder to the sink the layer recorder
// writes into, so that every input of "img layer" can be written as an image.
// What happens with identical files is up to the builder: ext4 stores files
// with the same content and metadata once, as hard links, like tar layers do.
// The content-addressed state shared between layers (--deduplicate) does not
// apply to images.
type imageSink struct {
	builder  imageBuilder
	metadata *LayerMetadata
}

var _ api.TarCAS = (*imageSink)(nil)

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

func (s *imageSink) WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error {
	return s.builder.WriteRegularFromPathDeduplicated(hdr, filePath)
}

func (s *imageSink) Import(api.CASStateSupplier) error { return nil }
func (s *imageSink) Export(api.CASStateExporter) error { return nil }

// ContentHasher is used by the recorder to digest tar entries before writing
// them. The ext4 builder takes SHA-256 digests.
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

func (s *imageSink) StoreTreeKnownHash(fsys fs.FS, intendedPath string, _ []byte) (string, error) {
	return s.StoreTree(fsys, intendedPath)
}

// StoreTree writes a directory tree (a tree artifact) below intendedPath. Like
// for tar layers, only regular files are stored, each with mode 0755 unless
// the layer metadata says otherwise. An empty link path is returned to tell the
// recorder that the contents have been written.
func (s *imageSink) StoreTree(fsys fs.FS, intendedPath string) (string, error) {
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
		return s.builder.WriteRegularDeduplicated(hdr, f)
	})
	return "", err
}

func (s *imageSink) applyMetadata(hdr *tar.Header) error {
	if s.metadata == nil {
		return nil
	}
	return s.metadata.applyToGeneratedHeader(hdr)
}

// writeImageMetadata writes the layer metadata file for an image layer. The
// digest and size describe the image file (the compressed one, if there is
// state from a compressor); other than that, the metadata is
// only as faithful as the layer rules need it to be, since image layers are not
// OCI layers.
func writeImageMetadata(image *os.File, size int64, state *api.AppenderState, history, defaultMediaType, mediaTypeOverride string, annotations map[string]string, output io.Writer) error {
	var digest, diffID string
	if state != nil {
		digest, diffID = fmt.Sprintf("sha256:%x", state.OuterHash), fmt.Sprintf("sha256:%x", state.ContentHash)
	} else {
		hasher := sha256.New()
		if _, err := io.Copy(hasher, io.NewSectionReader(image, 0, size)); err != nil {
			return fmt.Errorf("digesting image: %w", err)
		}
		digest = fmt.Sprintf("sha256:%x", hasher.Sum(nil))
		diffID = digest
	}

	var layerHistory []api.History
	if history == "" {
		layerHistory = []api.History{}
	} else {
		layerHistory = api.LayerHistory(history)
	}
	mediaType := defaultMediaType
	if mediaTypeOverride != "" {
		mediaType = mediaTypeOverride
	}
	return metadata.WriteLayerMetadata(diffID, mediaType, digest, size, annotations, layerHistory, output)
}
