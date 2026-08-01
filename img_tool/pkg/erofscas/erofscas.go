// Package erofscas records container image layer entries into an EROFS
// filesystem image.
//
// It is an alternative to [tarcas] behind the same [api.TarCAS] interface, so
// the same recorder drives either one: entries arrive as tar headers plus
// content, and what comes out is a layer blob. The difference is what the blob
// is. A tar layer is a changeset -- an ordered list of entries, where an absent
// directory means "do not touch it" -- while an EROFS layer is a filesystem
// image, which needs a complete directory tree and therefore synthesizes the
// parents a changeset may leave out. See docs/erofs-layers.md for what that
// implies for the layers below.
//
// The image is laid out metadata-first, so the blob is a superblock and metadata
// prefix followed by the input files' payloads, each a contiguous, block-aligned,
// verbatim copy. That shape is what lets a compact stream over an EROFS layer
// look exactly like a compact stream over a tar: scaffolding inline, file
// payloads as content-addressed references.
package erofscas

import (
	"archive/tar"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"iter"
	"os"
	"path"
	"strings"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tarmeta"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/merkle"
)

// OverlayOpaqueXattr marks a directory in an EROFS layer as opaque: entries of
// the same directory in lower layers are hidden. It is the overlayfs-native
// equivalent of a ".wh..wh..opq" marker file.
const OverlayOpaqueXattr = "trusted.overlay.opaque"

// AUFS whiteout markers, as they appear in a tar layer. An EROFS layer must not
// contain them: they are translated to their overlayfs equivalents.
const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// CAS records entries into an EROFS image, deduplicating repeated content.
type CAS[HM hashHelper] struct {
	w        *erofs.Writer
	appender io.Writer
	digestFS *digestfs.FileSystem

	hashOrder [][]byte
	nodeOrder [][]byte
	treeOrder [][]byte

	storedHashes map[string]struct{}
	storedNodes  map[string]struct{}
	storedTrees  map[string]struct{}

	firstBlobPaths map[string]string // blob hash -> first occurrence path
	firstNodePaths map[string]string // node hash -> first occurrence path
	firstTreePaths map[string]string // tree hash -> first occurrence path
	// firstPaths is the set of image paths recorded above, so replacing one --
	// which would leave the hardlinks pointing at it holding the wrong content
	// -- is caught instead of silently corrupting the layer. It is keyed by
	// image path rather than tar path so that two spellings of the same entry
	// ("etc/f" and "./etc/f") are recognized as the same path.
	firstPaths map[string]struct{}

	closed bool
	options
}

// New returns a CAS that writes its image to appender, which is the compression
// and hashing pipeline that produces the layer blob.
func New[HM hashHelper](appender io.Writer, digestFS *digestfs.FileSystem, opts ...Option) *CAS[HM] {
	options := defaultOptions()
	for _, opt := range opts {
		opt.apply(&options)
	}

	createOpts := []erofs.CreateOpt{erofs.WithBuildTime(options.buildTime, 0)}
	if options.blockSize > 0 {
		createOpts = append(createOpts, erofs.WithBlockSize(options.blockSize))
	}
	if options.inlineThreshold != 0 {
		createOpts = append(createOpts, erofs.WithInlineThreshold(int(options.inlineThreshold)))
	}
	if options.syntheticDirectories == SyntheticDirectoriesInherit {
		createOpts = append(createOpts, erofs.WithSyntheticDirMetadata(inheritFromAncestor))
	}

	if options.compactStreamWriter != nil {
		options.compactStreamWriter.SetContentFormat(compactstream.ContentFormatErofs)
	}

	var helper HM
	if digestFS == nil {
		digestFS = digestfs.New(helper)
	}

	return &CAS[HM]{
		w:              erofs.NewWriter(createOpts...),
		appender:       appender,
		digestFS:       digestFS,
		hashOrder:      [][]byte{},
		nodeOrder:      [][]byte{},
		treeOrder:      [][]byte{},
		storedHashes:   make(map[string]struct{}),
		storedNodes:    make(map[string]struct{}),
		storedTrees:    make(map[string]struct{}),
		firstBlobPaths: make(map[string]string),
		firstNodePaths: make(map[string]string),
		firstTreePaths: make(map[string]string),
		firstPaths:     make(map[string]struct{}),
		options:        options,
	}
}

// inheritFromAncestor gives a synthesized directory the attributes mkfs.erofs
// gives it (lib/rebuild.c): mode 0755 plus the nearest ancestor's group- and
// other-writable bits, with the ancestor's ownership and timestamp. Producing
// the same thing as the reference implementation means images converted by
// either producer agree.
func inheritFromAncestor(_ string, ancestor erofs.SyntheticDirMetadata) (erofs.SyntheticDirMetadata, bool) {
	return erofs.SyntheticDirMetadata{
		Mode:    0o755 | (ancestor.Mode & 0o022),
		UID:     ancestor.UID,
		GID:     ancestor.GID,
		Mtime:   ancestor.Mtime,
		MtimeNs: ancestor.MtimeNs,
	}, true
}

// SyntheticDirs returns the image paths of directories that had to be
// synthesized because an entry beneath them was recorded without an entry of
// their own. Their attributes come from the configured
// [SyntheticDirectories] policy rather than from the layer's inputs.
func (c *CAS[HM]) SyntheticDirs() []string {
	return c.w.SyntheticDirs()
}

// Close finalizes the layer: it plans the image layout, splits the byte stream
// into the compact stream if one was requested, and serializes the image into
// the appender.
func (c *CAS[HM]) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true

	layout, err := c.w.Prepare()
	if err != nil {
		return err
	}
	if err := c.reportSyntheticDirs(); err != nil {
		return err
	}

	dst := c.appender
	var splitter *compactstream.PlanSplitter
	if c.compactStreamWriter != nil {
		regions, err := regionsFromLayout(layout, c.compactStreamWriter.HashSize())
		if err != nil {
			return err
		}
		splitter, err = compactstream.NewPlanSplitter(c.compactStreamWriter, c.appender, regions, c.verifyCASRefs)
		if err != nil {
			return err
		}
		dst = splitter
	}

	if _, err := c.w.WriteTo(dst); err != nil {
		return fmt.Errorf("writing EROFS image: %w", err)
	}
	if splitter != nil {
		if err := splitter.Finish(layout.ImageSize); err != nil {
			return err
		}
	}
	return nil
}

// reportSyntheticDirs applies the configured policy to the directories the
// image had to invent.
func (c *CAS[HM]) reportSyntheticDirs() error {
	dirs := c.w.SyntheticDirs()
	if len(dirs) == 0 {
		return nil
	}
	if c.syntheticDirectories == SyntheticDirectoriesStrict {
		return fmt.Errorf("erofs layer: %d director%s must be synthesized because nothing describes them, and --erofs-synthetic-directories=strict forbids guessing their permissions and ownership: %s\n"+
			"Describe them with a base metadata stream, or add explicit directory entries",
			len(dirs), plural(len(dirs)), strings.Join(dirs, " "))
	}
	return nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// regionsFromLayout turns the file payloads of a planned image into the ranges
// the compact stream replaces with content-addressed references. Everything
// else -- the superblock, the metadata (inodes, xattrs, tail-packed small file
// bodies, inline dirents), out-of-line dirent blocks, symlink targets and all
// padding -- stays inline in the stream.
func regionsFromLayout(layout *erofs.Layout, hashSize int) ([]compactstream.Region, error) {
	var regions []compactstream.Region
	for _, e := range layout.Extents {
		if e.Kind != erofs.ExtentFileData {
			continue
		}
		digest, ok := e.Token.([]byte)
		if !ok || len(digest) != hashSize {
			return nil, fmt.Errorf("erofscas: no content digest recorded for %s (%d bytes at offset %d); every out-of-line payload needs one to become a CAS reference", e.Path, e.Size, e.Offset)
		}
		regions = append(regions, compactstream.Region{
			Offset: e.Offset,
			Size:   e.Size,
			Digest: digest,
		})
	}
	return regions, nil
}

// --- api.TarWriter ---

// WriteHeader records a non-regular entry: a directory, symlink, hardlink,
// device, or FIFO. AUFS whiteout markers are translated to their overlayfs
// equivalents (see [CAS.writeWhiteout]).
func (c *CAS[HM]) WriteHeader(hdr *tar.Header) error {
	if hdr.Typeflag == tar.TypeReg {
		return errors.New("WriteHeader called with regular file header, use WriteRegular instead")
	}

	norm := tarmeta.Clone(hdr)
	tarmeta.Normalize(&norm)
	// Normalization defines the content hash and folds extended attributes into
	// their PAX records, but it also rounds the timestamp to whole seconds. EROFS
	// stores nanoseconds, so the caller's timestamp is written verbatim and only
	// the hash sees the rounded one.
	norm.ModTime = hdr.ModTime

	if handled, err := c.writeWhiteout(&norm); handled || err != nil {
		return err
	}

	p, err := imagePath(norm.Name)
	if err != nil {
		return err
	}

	switch norm.Typeflag {
	case tar.TypeDir:
		if err := c.ensureDir(p, permMode(norm.Mode)); err != nil {
			return err
		}
	case tar.TypeSymlink:
		if err := c.replace(p); err != nil {
			return err
		}
		if err := c.w.Symlink(norm.Linkname, p); err != nil {
			return err
		}
	case tar.TypeLink:
		target, err := imagePath(norm.Linkname)
		if err != nil {
			return err
		}
		if err := c.replace(p); err != nil {
			return err
		}
		if _, err := c.w.Stat(target); err != nil {
			// EROFS hardlinks are real links within one image, so a link into
			// a lower layer cannot be represented at all.
			return fmt.Errorf("hardlink %s -> %s: the target is not part of this layer, and an EROFS layer cannot link across layers", norm.Name, norm.Linkname)
		}
		return c.w.Link(target, p)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		if err := c.replace(p); err != nil {
			return err
		}
		if err := c.w.Mknod(p, deviceType(norm.Typeflag)|(uint16(norm.Mode)&0o7777), encodeDev(norm.Devmajor, norm.Devminor)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("entry %s: tar type %q has no EROFS equivalent", norm.Name, string(norm.Typeflag))
	}

	return c.applyMetadata(p, &norm)
}

// whiteout translates hdr if it is an AUFS whiteout marker, reporting whether it
// was one. Markers are ordinary zero-size regular files in a tar layer, so every
// entry point has to check.
func (c *CAS[HM]) whiteout(hdr *tar.Header) (bool, error) {
	if !isWhiteout(hdr.Name) {
		return false, nil
	}
	norm := tarmeta.Clone(hdr)
	tarmeta.Normalize(&norm)
	return c.writeWhiteout(&norm)
}

// isWhiteout reports whether a tar entry name is an AUFS whiteout marker.
func isWhiteout(name string) bool {
	return strings.HasPrefix(path.Base(strings.TrimSuffix(name, "/")), whiteoutPrefix)
}

// writeWhiteout translates an AUFS whiteout marker into its overlayfs form,
// which is what the EROFS layer format uses: a deleted entry becomes a
// character device with rdev 0 in its place, and an opaque directory carries the
// trusted.overlay.opaque xattr. It reports whether hdr was such a marker; the
// marker file itself never reaches the image.
func (c *CAS[HM]) writeWhiteout(hdr *tar.Header) (bool, error) {
	name := strings.TrimSuffix(hdr.Name, "/")
	base := path.Base(name)
	if !strings.HasPrefix(base, whiteoutPrefix) {
		return false, nil
	}

	if base == whiteoutOpaque {
		dir, err := imagePath(path.Dir(name))
		if err != nil {
			return true, err
		}
		if err := c.ensureDirExists(dir); err != nil {
			return true, err
		}
		return true, c.w.Setxattr(dir, OverlayOpaqueXattr, "y")
	}

	deleted, err := imagePath(path.Join(path.Dir(name), strings.TrimPrefix(base, whiteoutPrefix)))
	if err != nil {
		return true, err
	}
	if err := c.replace(deleted); err != nil {
		return true, err
	}
	// The marker's permission bits are kept and only its type is replaced, which
	// is what mkfs.erofs does (lib/tar.c), so a layer converted by either
	// producer agrees.
	return true, c.w.Mknod(deleted, erofs.ModeChardev|uint16(hdr.Mode&0o7777), 0)
}

func (c *CAS[HM]) WriteRegular(hdr *tar.Header, r io.Reader) error {
	if handled, err := c.whiteout(hdr); handled || err != nil {
		return err
	}
	norm, p, err := c.regularHeader(hdr, "WriteRegular")
	if err != nil {
		return err
	}
	if _, _, err := c.spool(p, norm.Size, r); err != nil {
		return err
	}
	return c.applyMetadata(p, norm)
}

func (c *CAS[HM]) WriteRegularFromPath(hdr *tar.Header, filePath string) error {
	if handled, err := c.whiteout(hdr); handled || err != nil {
		return err
	}
	norm, p, err := c.regularHeader(hdr, "WriteRegularFromPath")
	if err != nil {
		return err
	}
	digest, size, err := c.digestOf(filePath)
	if err != nil {
		return err
	}
	if size != norm.Size {
		return fmt.Errorf("expected file of size %d, got %d", norm.Size, size)
	}
	if err := c.addFile(p, size, filePath, digest); err != nil {
		return err
	}
	return c.applyMetadata(p, norm)
}

func (c *CAS[HM]) WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error {
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("WriteRegularDeduplicated called with non-regular header: %s", hdr.Name)
	}
	if handled, err := c.whiteout(hdr); handled || err != nil {
		return err
	}

	var linkPath string
	var sz int64
	var storeErr error
	if tarmeta.IsBlob(hdr) {
		linkPath, _, sz, storeErr = c.Store(r, hdr.Name)
	} else {
		linkPath, _, sz, storeErr = c.StoreNode(r, hdr)
	}
	if storeErr != nil {
		return storeErr
	}
	if sz != hdr.Size {
		return fmt.Errorf("expected file of size %d, got %d", hdr.Size, sz)
	}
	return c.linkDuplicate(linkPath, hdr)
}

func (c *CAS[HM]) WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error {
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("WriteRegularFromPathDeduplicated called with non-regular header: %s", hdr.Name)
	}
	if handled, err := c.whiteout(hdr); handled || err != nil {
		return err
	}
	digest, size, err := c.digestOf(filePath)
	if err != nil {
		return err
	}
	if size != hdr.Size {
		return fmt.Errorf("expected file of size %d, got %d", hdr.Size, size)
	}

	var linkPath string
	if tarmeta.IsBlob(hdr) {
		linkPath, err = c.storeBlob(digest, size, hdr.Name, fromPath(filePath))
	} else {
		linkPath, err = c.storeNode(hdr, digest, fromPath(filePath))
	}
	if err != nil {
		return err
	}
	return c.linkDuplicate(linkPath, hdr)
}

// WriteRegularDeduplicatedKnownHash records a regular file whose content digest
// the caller already knows, streaming r straight into the layer.
//
// The digest is what decides whether an entry becomes a content-addressed object
// or a link to one, so it has to be known before anything is written. A caller
// reading from a source it cannot rewind -- a tar stream, say -- would otherwise
// have to hold the whole entry in memory to hash it first; this lets it digest
// the content in an earlier pass instead. r must yield exactly hdr.Size bytes
// hashing to blobHash.
func (c *CAS[HM]) WriteRegularDeduplicatedKnownHash(hdr *tar.Header, r io.Reader, blobHash []byte) error {
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("WriteRegularDeduplicatedKnownHash called with non-regular header: %s", hdr.Name)
	}
	if handled, err := c.whiteout(hdr); handled || err != nil {
		return err
	}

	var linkPath string
	var err error
	if tarmeta.IsBlob(hdr) {
		linkPath, err = c.storeBlob(blobHash, hdr.Size, hdr.Name, fromReader(r))
	} else {
		linkPath, err = c.storeNode(hdr, blobHash, fromReader(r))
	}
	if err != nil {
		return err
	}
	return c.linkDuplicate(linkPath, hdr)
}

// linkDuplicate adds the dirent for an entry whose content and metadata were
// already recorded at linkPath. A tar layer expresses this with a hardlink
// entry, and so does an EROFS layer, which keeps the two formats' link counts
// (and therefore their mtree renderings) identical.
func (c *CAS[HM]) linkDuplicate(linkPath string, hdr *tar.Header) error {
	if linkPath == "" || linkPath == hdr.Name {
		// The entry was written at its own path.
		return nil
	}
	target, err := imagePath(linkPath)
	if err != nil {
		return err
	}
	p, err := imagePath(hdr.Name)
	if err != nil {
		return err
	}
	if err := c.replace(p); err != nil {
		return err
	}
	return c.w.Link(target, p)
}

// --- api.CAS ---

func (c *CAS[HM]) Import(from api.CASStateSupplier) error {
	for hash, err := range from.BlobHashes() {
		if err != nil {
			return err
		}
		c.storedHashes[string(hash)] = struct{}{}
	}
	for hash, err := range from.NodeHashes() {
		if err != nil {
			return err
		}
		c.storedNodes[string(hash)] = struct{}{}
	}
	for hash, err := range from.TreeHashes() {
		if err != nil {
			return err
		}
		c.storedTrees[string(hash)] = struct{}{}
	}
	return nil
}

func (c *CAS[HM]) Export(to api.CASStateExporter) error {
	return to.Export(&exporterState{
		hashOrder: c.hashOrder,
		nodeOrder: c.nodeOrder,
		treeOrder: c.treeOrder,
	})
}

// ContentHasher returns a new hasher for the CAS's hash algorithm, so callers
// that have to digest content before handing it over produce digests this CAS
// accepts.
func (c *CAS[HM]) ContentHasher() hash.Hash {
	var helper HM
	return helper.New()
}

func (c *CAS[HM]) Store(r io.Reader, intendedPath string) (string, []byte, int64, error) {
	p, err := imagePath(intendedPath)
	if err != nil {
		return "", nil, 0, err
	}
	if err := c.replace(p); err != nil {
		return "", nil, 0, err
	}
	// The digest is only known once the content has been read, and the reader
	// cannot be rewound, so the payload is spooled first and dropped again if
	// it turns out to be a duplicate.
	digest, size, err := c.spool(p, -1, r)
	if err != nil {
		return "", nil, size, err
	}
	if firstPath, exists := c.firstBlobPaths[string(digest)]; exists {
		if err := c.w.Remove(p); err != nil {
			return "", nil, size, err
		}
		return firstPath, digest, size, nil
	}
	if err := c.w.Chmod(p, blobMode); err != nil {
		return "", nil, size, err
	}
	c.recordBlob(digest, intendedPath)
	return intendedPath, digest, size, nil
}

func (c *CAS[HM]) StoreKnownHashAndSize(r io.Reader, blobHash []byte, size int64, intendedPath string) (string, error) {
	if firstPath, exists := c.firstBlobPaths[string(blobHash)]; exists {
		return firstPath, nil
	}
	return c.storeBlob(blobHash, size, intendedPath, fromReader(r))
}

func (c *CAS[HM]) StoreNode(r io.Reader, hdr *tar.Header) (string, []byte, int64, error) {
	norm, p, err := c.regularHeader(hdr, "StoreNode")
	if err != nil {
		return "", nil, 0, err
	}
	digest, size, err := c.spool(p, norm.Size, r)
	if err != nil {
		return "", nil, size, err
	}
	nodeHash := c.nodeHash(hdr, digest)
	if firstPath, exists := c.firstNodePaths[string(nodeHash)]; exists {
		if err := c.w.Remove(p); err != nil {
			return "", nil, size, err
		}
		return firstPath, digest, size, nil
	}
	if err := c.applyMetadata(p, norm); err != nil {
		return "", nil, size, err
	}
	c.recordNode(nodeHash, hdr.Name)
	return hdr.Name, digest, size, nil
}

func (c *CAS[HM]) StoreNodeKnownHash(r io.Reader, hdr *tar.Header, blobHash []byte) (string, error) {
	nodeHash := c.nodeHash(hdr, blobHash)
	if firstPath, exists := c.firstNodePaths[string(nodeHash)]; exists {
		return firstPath, nil
	}
	return c.storeNode(hdr, blobHash, fromReader(r))
}

func (c *CAS[HM]) StoreTree(fsys fs.FS, intendedPath string) (string, error) {
	// A content-only tree hash cannot represent path-specific metadata, so a
	// layer that configures any is not eligible for whole-tree deduplication.
	// Its files still deduplicate individually.
	if c.generatedMetadata != nil {
		return c.StoreTreeKnownHash(fsys, intendedPath, nil)
	}
	var helper HM
	treeHasher := merkle.NewTreeHasher(fsys, helper.New)
	rootHash, err := treeHasher.Build()
	if err != nil {
		return "", fmt.Errorf("calculating tree hash before storing tree artifact in layer: %w", err)
	}
	return c.StoreTreeKnownHash(fsys, intendedPath, rootHash)
}

func (c *CAS[HM]) StoreTreeKnownHash(fsys fs.FS, intendedPath string, treeHash []byte) (string, error) {
	hashStr := string(treeHash)
	if treeBase, exists := c.firstTreePaths[hashStr]; exists && c.deduplicateTreeArtifacts && c.generatedMetadata == nil {
		return treeBase, nil
	}

	root, err := imagePath(intendedPath)
	if err != nil {
		return "", err
	}
	if err := c.ensureDir(root, treeDirMode); err != nil {
		return "", err
	}
	if c.generatedMetadata != nil {
		hdr := &tar.Header{
			Typeflag: tar.TypeDir,
			Name:     intendedPath + "/",
			Mode:     int64(treeDirMode),
		}
		if err := c.generatedMetadata(hdr); err != nil {
			return "", err
		}
		if err := c.applyMetadata(root, hdr); err != nil {
			return "", err
		}
	}

	if err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking directory %s: %w", p, err)
		}
		if !d.Type().IsRegular() {
			// Directories are created implicitly by the files beneath them,
			// and a tree artifact carries no other entry types.
			return nil
		}
		f, err := fsys.Open(p)
		if err != nil {
			return fmt.Errorf("opening file %s: %w", p, err)
		}
		defer f.Close()
		treePath := path.Join(intendedPath, p)
		if c.generatedMetadata != nil {
			info, err := f.Stat()
			if err != nil {
				return err
			}
			hdr := &tar.Header{
				Typeflag: tar.TypeReg,
				Name:     treePath,
				Size:     info.Size(),
				Mode:     int64(blobMode),
			}
			if err := c.generatedMetadata(hdr); err != nil {
				return err
			}
			// The metadata is part of what identifies the entry, so this goes
			// through the node path rather than the content-only blob path.
			return c.WriteRegularDeduplicated(hdr, f)
		}
		linkName, _, _, err := c.Store(f, treePath)
		if err != nil {
			return fmt.Errorf("storing file %s: %w", p, err)
		}
		return c.linkDuplicate(linkName, &tar.Header{Name: treePath})
	}); err != nil {
		return "", fmt.Errorf("storing tree artifact %x in layer: %w", treeHash, err)
	}

	if c.generatedMetadata != nil {
		return "", nil
	}
	c.storedTrees[hashStr] = struct{}{}
	c.firstTreePaths[hashStr] = intendedPath
	c.treeOrder = append(c.treeOrder, treeHash)
	return "", nil
}

// --- internals ---

// blobMode is the mode a content-addressed entry is stored with: an entry that
// qualifies for blob deduplication carries no metadata of its own, so its mode
// is fixed. It must match what tarcas writes, or the two sinks' content
// manifests stop being interchangeable.
const blobMode fs.FileMode = 0o755

// treeDirMode is the mode of the root directory of a stored tree artifact,
// matching tarcas.
const treeDirMode fs.FileMode = 0o755

// storeBlob records a content-addressed payload, using the fixed metadata a
// blob entry has. A payload already stored under the same digest is not stored
// again; its first path is returned instead.
func (c *CAS[HM]) storeBlob(digest []byte, size int64, intendedPath string, src source) (string, error) {
	if firstPath, exists := c.firstBlobPaths[string(digest)]; exists {
		return firstPath, nil
	}
	p, err := imagePath(intendedPath)
	if err != nil {
		return "", err
	}
	if err := c.replace(p); err != nil {
		return "", err
	}
	if err := c.write(p, size, src, digest); err != nil {
		return "", err
	}
	if err := c.w.Chmod(p, blobMode); err != nil {
		return "", err
	}
	c.recordBlob(digest, intendedPath)
	return intendedPath, nil
}

// storeNode records an entry identified by both its metadata and its content. An
// entry already stored with the same metadata and content is not stored again;
// its first path is returned instead.
func (c *CAS[HM]) storeNode(hdr *tar.Header, digest []byte, src source) (string, error) {
	nodeHash := c.nodeHash(hdr, digest)
	if firstPath, exists := c.firstNodePaths[string(nodeHash)]; exists {
		return firstPath, nil
	}
	norm, p, err := c.regularHeader(hdr, "storeNode")
	if err != nil {
		return "", err
	}
	if err := c.write(p, norm.Size, src, digest); err != nil {
		return "", err
	}
	if err := c.applyMetadata(p, norm); err != nil {
		return "", err
	}
	c.recordNode(nodeHash, hdr.Name)
	return hdr.Name, nil
}

func (c *CAS[HM]) recordBlob(digest []byte, entryPath string) {
	c.storedHashes[string(digest)] = struct{}{}
	c.firstBlobPaths[string(digest)] = entryPath
	c.recordFirstPath(entryPath)
	c.hashOrder = append(c.hashOrder, digest)
}

func (c *CAS[HM]) recordNode(nodeHash []byte, entryPath string) {
	c.storedNodes[string(nodeHash)] = struct{}{}
	c.firstNodePaths[string(nodeHash)] = entryPath
	c.recordFirstPath(entryPath)
	c.nodeOrder = append(c.nodeOrder, nodeHash)
}

// recordFirstPath notes that entryPath (a tar entry name) holds the first copy
// of some content, so later duplicates hardlink to it.
func (c *CAS[HM]) recordFirstPath(entryPath string) {
	if p, err := imagePath(entryPath); err == nil {
		c.firstPaths[p] = struct{}{}
	}
}

// nodeHash is the hash identifying an entry by its normalized metadata plus its
// content digest. The definition is shared with tarcas so a content manifest
// written by one sink means the same thing to the other.
func (c *CAS[HM]) nodeHash(hdr *tar.Header, blobHash []byte) []byte {
	var helper HM
	recorded := tarmeta.Clone(hdr)
	recorded.Name = ""
	tarmeta.Normalize(&recorded)

	hasher := helper.New()
	tarmeta.Hash(hasher, recorded)
	hasher.Write(blobHash)
	return hasher.Sum(nil)
}

// regularHeader normalizes a regular file's header, resolves its image path and
// makes that path available for a new entry.
func (c *CAS[HM]) regularHeader(hdr *tar.Header, caller string) (*tar.Header, string, error) {
	if hdr.Typeflag != tar.TypeReg {
		return nil, "", fmt.Errorf("%s called with non-regular header: %s", caller, hdr.Name)
	}
	norm := tarmeta.Clone(hdr)
	tarmeta.Normalize(&norm)
	// See WriteHeader: the rounded timestamp is for hashing only.
	norm.ModTime = hdr.ModTime
	p, err := imagePath(norm.Name)
	if err != nil {
		return nil, "", err
	}
	if err := c.replace(p); err != nil {
		return nil, "", err
	}
	return &norm, p, nil
}

// source is where a regular file's content comes from: a path on disk, which
// can be read lazily at serialization time, or a reader that has to be consumed
// now.
type source struct {
	path   string
	reader io.Reader
}

func fromPath(p string) source      { return source{path: p} }
func fromReader(r io.Reader) source { return source{reader: r} }

// write records a regular file's content from src, whose digest is already
// known.
func (c *CAS[HM]) write(p string, size int64, src source, digest []byte) error {
	if src.path != "" {
		return c.addFile(p, size, src.path, digest)
	}
	got, _, err := c.spool(p, size, src.reader)
	if err != nil {
		return err
	}
	if string(got) != string(digest) {
		return fmt.Errorf("%s: content digest %x does not match the expected %x", p, got, digest)
	}
	return nil
}

// addFile records a regular file whose content lives at filePath. The file is
// not read now: the reader opens it when the image is serialized, so a layer
// with many files needs neither a spool copy nor thousands of open descriptors.
//
// digest was computed from a separate, earlier read of the same path, so this
// relies on the file not changing in between -- which is the same contract every
// build action already depends on, and which tarcas relies on too through the
// shared digest cache. For a payload large enough to get an extent of its own the
// splitter re-hashes it as it is written and fails on a mismatch (see
// VerifyCASRefs); a tail-packed payload has no such check.
func (c *CAS[HM]) addFile(p string, size int64, filePath string, digest []byte) error {
	var content io.Reader
	if size > 0 {
		content = &lazyFile{path: filePath}
	}
	if err := c.w.AddFile(p, size, content); err != nil {
		return err
	}
	return c.w.SetToken(p, digest)
}

// spool records a regular file by copying r into the writer's spool, hashing it
// on the way. size may be -1 when it is not known in advance.
func (c *CAS[HM]) spool(p string, size int64, r io.Reader) ([]byte, int64, error) {
	var helper HM
	h := helper.New()

	if r == nil && size > 0 {
		return nil, 0, fmt.Errorf("%s: declared %d bytes of content but no reader was given", p, size)
	}
	if size == 0 || r == nil {
		if err := c.w.AddFile(p, 0, nil); err != nil {
			return nil, 0, err
		}
		digest := h.Sum(nil)
		return digest, 0, c.w.SetToken(p, digest)
	}

	if size > 0 {
		r = io.LimitReader(r, size)
	}
	f, err := c.w.Create(p)
	if err != nil {
		return nil, 0, err
	}
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		_ = f.Close()
		return nil, n, err
	}
	if err := f.Close(); err != nil {
		return nil, n, err
	}
	if size > 0 && n != size {
		return nil, n, fmt.Errorf("%s: expected %d bytes of content, got %d", p, size, n)
	}
	digest := h.Sum(nil)
	return digest, n, c.w.SetToken(p, digest)
}

// digestOf returns a file's content digest and size, reusing the shared digest
// cache so the content is hashed at most once per build.
func (c *CAS[HM]) digestOf(filePath string) ([]byte, int64, error) {
	df, err := c.digestFS.OpenFile(filePath)
	if err != nil {
		return nil, 0, err
	}
	defer df.Close()
	digest, err := df.Digest()
	if err != nil {
		return nil, 0, err
	}
	return digest, df.Size(), nil
}

// ensureDir creates a directory, or updates the permissions of one that already
// exists (which happens when an entry beneath it was recorded first, or when a
// whiteout marker referred to it).
func (c *CAS[HM]) ensureDir(p string, perm fs.FileMode) error {
	if existing, err := c.w.Stat(p); err == nil && !existing.IsDir() {
		if err := c.replace(p); err != nil {
			return err
		}
	}
	return c.w.Mkdir(p, perm)
}

// ensureDirExists makes sure a directory is there without claiming to describe
// it, so it never overwrites permissions an explicit entry has already set. A
// directory that only exists because of this is invented, not described, so it
// gets the configured synthesized-directory attributes and is reported alongside
// the invented parents.
func (c *CAS[HM]) ensureDirExists(p string) error {
	if existing, err := c.w.Stat(p); err == nil {
		if !existing.IsDir() {
			return fmt.Errorf("%s: expected a directory, found %s", p, existing.Mode().Type())
		}
		return nil
	}
	return c.w.MkdirSynthesized(p)
}

// replace makes p available for a new entry by dropping whatever the layer
// recorded there before. A tar layer would carry both entries and let the
// extractor apply the last one; a filesystem image has to resolve the conflict
// while it is being built.
func (c *CAS[HM]) replace(p string) error {
	if _, err := c.w.Stat(p); err != nil {
		return nil
	}
	if _, ok := c.firstPaths[p]; ok {
		return fmt.Errorf("%s is recorded twice in this layer, and the first entry is the one deduplicated copies of it link to; give the second entry a different path", tarPath(p))
	}
	return c.w.Remove(p)
}

// applyMetadata sets an entry's mode, ownership, timestamp and extended
// attributes from its tar header.
func (c *CAS[HM]) applyMetadata(p string, hdr *tar.Header) error {
	// The header's mode is applied verbatim, including to symlinks, whose
	// permission bits the kernel ignores but which mkfs.erofs also copies from
	// its source -- so an image converted by either producer agrees.
	if err := c.w.Chmod(p, permMode(hdr.Mode)); err != nil {
		return err
	}
	// Only the numeric ids: an EROFS inode has no place for the owner names a
	// tar header can carry, which is also true of the kernel's own inodes.
	if err := c.w.Chown(p, hdr.Uid, hdr.Gid); err != nil {
		return err
	}
	// An unset mtime stays zero rather than becoming the huge value a zero
	// time.Time's Unix() would produce, which also keeps the inode compact.
	if !hdr.ModTime.IsZero() {
		if err := c.w.Chtimes(p, hdr.ModTime, hdr.ModTime); err != nil {
			return err
		}
	}
	for name, value := range xattrs(hdr) {
		if err := c.w.Setxattr(p, name, value); err != nil {
			return err
		}
	}
	return nil
}

// xattrs returns the extended attributes a normalized header carries. Normalize
// folds tar's deprecated Xattrs field into the SCHILY.xattr.* PAX records, so
// only those need reading.
func xattrs(hdr *tar.Header) map[string]string {
	const prefix = "SCHILY.xattr."
	var out map[string]string
	for k, v := range hdr.PAXRecords {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[strings.TrimPrefix(k, prefix)] = v
	}
	return out
}

// imagePath converts a tar entry name into an absolute path in the image.
func imagePath(name string) (string, error) {
	cleaned := path.Clean("/" + strings.TrimSuffix(name, "/"))
	if cleaned == "/" {
		// The image root always exists; an entry describing it is applied to it.
		return "/", nil
	}
	if strings.HasPrefix(cleaned, "/../") {
		return "", fmt.Errorf("entry %q escapes the image root", name)
	}
	return cleaned, nil
}

// tarPath is the inverse of imagePath, for error messages and for the
// deduplication bookkeeping, which is kept in tar-path space so content
// manifests stay comparable with tarcas.
func tarPath(p string) string {
	return strings.TrimPrefix(p, "/")
}

// permMode converts a tar header's mode into the permission bits, including
// setuid, setgid and sticky.
func permMode(mode int64) fs.FileMode {
	m := fs.FileMode(mode & 0o777)
	if mode&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// deviceType maps a tar special-file type to the EROFS inode type bits.
func deviceType(typeflag byte) uint16 {
	switch typeflag {
	case tar.TypeChar:
		return erofs.ModeChardev
	case tar.TypeBlock:
		return erofs.ModeBlockdev
	default:
		return erofs.ModeFifo
	}
}

// encodeDev packs a device's major and minor numbers the way the kernel's
// new_encode_dev does, which is how EROFS stores i_rdev.
func encodeDev(major, minor int64) uint32 {
	return uint32(major&0xfff)<<8 | uint32(minor&0xff) | uint32(minor&^0xff)<<12
}

// lazyFile reads a file that is opened on first use and closed once it has been
// consumed, so content recorded early can be read late without holding a
// descriptor in between.
type lazyFile struct {
	path string
	f    *os.File
}

func (l *lazyFile) Read(p []byte) (int, error) {
	if l.f == nil {
		f, err := os.Open(l.path)
		if err != nil {
			return 0, err
		}
		l.f = f
	}
	return l.f.Read(p)
}

func (l *lazyFile) Close() error {
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	return f.Close()
}

type hashHelper interface {
	New() hash.Hash
}

type exporterState struct {
	hashOrder [][]byte
	nodeOrder [][]byte
	treeOrder [][]byte
}

func (e *exporterState) BlobHashes() iter.Seq2[[]byte, error] {
	return sequence(e.hashOrder)
}

func (e *exporterState) NodeHashes() iter.Seq2[[]byte, error] {
	return sequence(e.nodeOrder)
}

func (e *exporterState) TreeHashes() iter.Seq2[[]byte, error] {
	return sequence(e.treeOrder)
}

func sequence(hashes [][]byte) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		for _, hash := range hashes {
			if !yield(hash, nil) {
				return
			}
		}
	}
}
