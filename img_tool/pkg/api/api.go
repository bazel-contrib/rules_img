package api

import (
	"archive/tar"
	"io"
	"io/fs"
	"iter"
)

type (
	CompressionAlgorithm string
	HashAlgorithm        string
	LayerFormat          string
)

const (
	// Compression algorithms
	Uncompressed CompressionAlgorithm = "uncompressed"
	Gzip         CompressionAlgorithm = "gzip"
	Zstd         CompressionAlgorithm = "zstd"

	// Hash algorithms
	SHA256 HashAlgorithm = "sha256"

	// Layer formats
	TarLayer     = "application/vnd.oci.image.layer.v1.tar"
	TarGzipLayer = "application/vnd.oci.image.layer.v1.tar+gzip"
	TarZstdLayer = "application/vnd.oci.image.layer.v1.tar+zstd"
)

func (h HashAlgorithm) Len() int {
	switch h {
	case SHA256:
		return 32
	default:
		return 0
	}
}

func (c LayerFormat) CompressionAlgorithm() CompressionAlgorithm {
	switch c {
	case TarLayer:
		return Uncompressed
	case TarGzipLayer:
		return Gzip
	case TarZstdLayer:
		return Zstd
	default:
		return ""
	}
}

type FileType struct{ inner string }

func (f FileType) String() string {
	return f.inner
}

var (
	RegularFile = FileType{"f"}
	Directory   = FileType{"d"}
	Symlink     = FileType{"l"}
)

type Descriptor struct {
	Name        string            `json:"name,omitempty"`
	DiffID      string            `json:"diff_id,omitempty"`
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type AppenderState struct {
	// Magic is an identifier for the format of the state.
	Magic string `json:"magic"`
	// OuterHashState is the inner state of the hash for the compressed data.
	// Used to resume the hash function for appending.
	OuterHashState []byte `json:"outer_hash_state"`
	// OuterHash is the final hash for the compressed data.
	// Cannot be used for resuming, but is the actual hash.
	OuterHash []byte `json:"outer_hash"`
	// ContentHashState is the state of the hash for the inner, uncompressed data.
	ContentHashState []byte `json:"content_hash_state"`
	// ContentHash is the final hash for the inner, uncompressed data.
	ContentHash []byte `json:"content_hash"`
	// CompressedSize is the compressed size of the blob.
	CompressedSize int64 `json:"compressed_size"`
	// UncompressedSize is the uncompressed size of the blob.
	UncompressedSize int64 `json:"uncompressed_size"`
	// LayerAnnotations are additional metadata for the layer.
	LayerAnnotations map[string]string `json:"layer_annotations,omitempty"`
}

type Appender interface {
	io.Writer
	Finalize() (AppenderState, error)
}

type TarAppender interface {
	AppendTar(r io.Reader) error
	Finalize() (AppenderState, error)
}

const (
	// TocDigestAnnotation is the annotation key for the TOC digest in estargz layers
	TocDigestAnnotation = "containerd.io/snapshot/stargz/toc.digest"
	// UncompressedSizeAnnotation is the annotation key for the uncompressed size in estargz layers
	UncompressedSizeAnnotation = "io.containers.estargz.uncompressed-size"
)

type CAS interface {
	Import(CASStateSupplier) error
	Export(CASStateExporter) error
	Store(r io.Reader) (linkPath string, blobHash []byte, blobSize int64, err error)
	StoreKnownHashAndSize(r io.Reader, blobHash []byte, size int64) (linkPath string, err error)
	StoreNode(r io.Reader, hdr *tar.Header) (linkPath string, blobHash []byte, size int64, err error)
	StoreNodeKnownHash(r io.Reader, hdr *tar.Header, blobHash []byte) (linkPath string, err error)
	StoreTree(fsys fs.FS) (linkPath string, err error)
	StoreTreeKnownHash(fsys fs.FS, treeHash []byte) (linkPath string, err error)
}

type CASStateSupplier interface {
	// Blobs are files without any metadata.
	// The hash is the hash of the file contents.
	BlobHashes() iter.Seq2[[]byte, error]
	// Nodes are inodes with metadata.
	// The hash includes any metadata,
	// as well as the file contents.
	NodeHashes() iter.Seq2[[]byte, error]
	// Trees are made up of blobs
	// with paths in the tree.
	TreeHashes() iter.Seq2[[]byte, error]
}

type CASStateExporter interface {
	Export(CASStateSupplier) error
}

type TarWriter interface {
	Close() error
	WriteHeader(hdr *tar.Header) error
	WriteRegular(hdr *tar.Header, r io.Reader) error
	WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error
	WriteRegularFromPath(hdr *tar.Header, filePath string) error
	WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error
}

type TarCAS interface {
	CAS
	TarWriter
}
