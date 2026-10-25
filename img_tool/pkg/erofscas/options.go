package erofscas

import (
	"archive/tar"
	"crypto/sha256"
	"errors"
	"hash"
	"io"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
)

// DefaultBlockSize is the filesystem block size an EROFS layer uses when the
// caller does not choose one. It matches the common page size, so an image can
// be mounted without kernel support for block sizes below the page size.
const DefaultBlockSize = 4096

// SyntheticDirectories selects the attributes given to a directory the image has
// to invent because nothing in the layer describes it.
//
// An EROFS layer is a filesystem, so its directory tree must be complete: a
// layer that writes /foo/bar/baz without describing /foo needs a /foo inode
// regardless. Whatever attributes that inode gets shadow the same directory in
// any lower layer, because overlayfs reports directory metadata from the upper
// layer only. See docs/erofs-layers.md.
type SyntheticDirectories int

const (
	// SyntheticDirectoriesInherit gives a synthesized directory mode 0755 plus
	// the nearest ancestor's group- and other-writable bits, with the
	// ancestor's ownership and timestamp -- what mkfs.erofs does.
	SyntheticDirectoriesInherit SyntheticDirectories = iota
	// SyntheticDirectoriesStrict fails the layer instead of guessing, so a
	// directory whose permissions matter has to be described.
	SyntheticDirectoriesStrict
	// SyntheticDirectoriesDefault gives a synthesized directory 0755 owned by
	// root, ignoring its surroundings.
	SyntheticDirectoriesDefault
)

// Option configures a CAS.
type Option interface {
	apply(*options)
}

// GeneratedMetadata applies metadata to the entries of a stored tree artifact,
// which nothing above the sink describes: a tree arrives as a filesystem, not as
// a list of headers, so the metadata a layer configures for the paths inside it
// can only be applied here. Regular-file metadata is applied before
// deduplication. Mirrors [tarcas.GeneratedMetadata], so the two sinks give a
// tree the same attributes.
//
// Synthesized parent directories are not routed through this hook: an EROFS
// layer invents them while it plans the image rather than recording them as
// entries, and their attributes come from the [SyntheticDirectories] policy.
type GeneratedMetadata func(hdr *tar.Header) error

func (m GeneratedMetadata) apply(o *options) { o.generatedMetadata = m }

type options struct {
	compactStreamWriter      *compactstream.Writer
	verifyCASRefs            bool
	inlineThreshold          int64
	blockSize                int
	buildTime                uint64
	syntheticDirectories     SyntheticDirectories
	deduplicateTreeArtifacts bool
	generatedMetadata        GeneratedMetadata
}

func defaultOptions() options {
	return options{
		verifyCASRefs: true,
		blockSize:     DefaultBlockSize,
	}
}

// WithCompactStreamWriter makes the CAS emit a compact stream describing the
// image alongside it: the scaffolding inline, each file payload as a single
// content-addressed reference.
type WithCompactStreamWriter struct{ Writer *compactstream.Writer }

func (w WithCompactStreamWriter) apply(o *options) { o.compactStreamWriter = w.Writer }

// VerifyCASRefs controls whether each region the compact stream replaces with a
// reference is re-hashed as it is written and checked against the recorded
// digest. On by default: the bytes are already in hand, so this turns a wrong
// layout plan into a build failure instead of a compact stream that only fails
// when someone reconstructs the layer.
type VerifyCASRefs bool

func (v VerifyCASRefs) apply(o *options) { o.verifyCASRefs = bool(v) }

// InlineThreshold caps tail-packing of file payloads into inodes: a file at
// least this large gets a data extent of its own, and therefore becomes a CAS
// reference in the compact stream rather than inline bytes. Zero leaves the
// decision to whatever fits in the inode's block.
type InlineThreshold int64

func (t InlineThreshold) apply(o *options) { o.inlineThreshold = int64(t) }

// BlockSize sets the filesystem block size.
type BlockSize int

func (b BlockSize) apply(o *options) { o.blockSize = int(b) }

// BuildTime sets the image's build timestamp. It doubles as the reference mtime
// that lets an inode use the compact 32-byte form, so the default of zero --
// which matches the layer default of an unset mtime -- produces the smallest
// and most reproducible images.
type BuildTime uint64

func (b BuildTime) apply(o *options) { o.buildTime = uint64(b) }

// WithSyntheticDirectories selects the policy for directories the image has to
// invent.
type WithSyntheticDirectories SyntheticDirectories

func (s WithSyntheticDirectories) apply(o *options) {
	o.syntheticDirectories = SyntheticDirectories(s)
}

// DeduplicateTreeArtifacts replaces a repeated tree artifact with a link to the
// first copy instead of storing it again.
type DeduplicateTreeArtifacts bool

func (d DeduplicateTreeArtifacts) apply(o *options) { o.deduplicateTreeArtifacts = bool(d) }

// SHA256Helper builds the SHA-256 hashes used for content and node digests.
type SHA256Helper struct{}

func (SHA256Helper) New() hash.Hash {
	return sha256.New()
}

// NewSHA256CASWithDigestFS returns a SHA-256 CAS writing its image to appender.
func NewSHA256CASWithDigestFS(appender io.Writer, digestFS *digestfs.FileSystem, opts ...Option) *CAS[SHA256Helper] {
	return New[SHA256Helper](appender, digestFS, opts...)
}

// CASFactoryWithDigestFS returns a CAS for the named hash algorithm.
func CASFactoryWithDigestFS(hashAlgorithm string, appender io.Writer, digestFS *digestfs.FileSystem, opts ...Option) (api.TarCAS, error) {
	switch hashAlgorithm {
	case "sha256":
		return NewSHA256CASWithDigestFS(appender, digestFS, opts...), nil
	}
	return nil, errors.New("unsupported hash algorithm")
}
