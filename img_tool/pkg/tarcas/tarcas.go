package tarcas

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"iter"
	"os"
	"path"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/tree/merkle"
)

type CAS[HM hashHelper] struct {
	deferredFiles []*tar.Header
	tarAppender   api.TarAppender
	hashOrder     [][]byte
	nodeOrder     [][]byte
	treeOrder     [][]byte
	storedHashes  map[string]struct{}
	storedNodes   map[string]struct{}
	storedTrees   map[string]struct{}
	closed        bool
	digestFS      *digestfs.FileSystem
	options
}

func New[HM hashHelper](appender api.TarAppender, opts ...Option) *CAS[HM] {
	options := options{
		structure:                 CASFirst,
		writeHeaderCallbackFilter: WriteHeaderCallbackFilterDefault,
	}
	for _, opt := range opts {
		opt.apply(&options)
	}

	var helper HM
	return &CAS[HM]{
		tarAppender:  appender,
		hashOrder:    [][]byte{},
		nodeOrder:    [][]byte{},
		treeOrder:    [][]byte{},
		storedHashes: make(map[string]struct{}),
		storedNodes:  make(map[string]struct{}),
		storedTrees:  make(map[string]struct{}),
		digestFS:     digestfs.New(helper),
		options:      options,
	}
}

func NewWithDigestFS[HM hashHelper](appender api.TarAppender, digestFS *digestfs.FileSystem, opts ...Option) *CAS[HM] {
	options := options{
		structure:                 CASFirst,
		writeHeaderCallbackFilter: WriteHeaderCallbackFilterDefault,
	}
	for _, opt := range opts {
		opt.apply(&options)
	}

	return &CAS[HM]{
		tarAppender:  appender,
		hashOrder:    [][]byte{},
		nodeOrder:    [][]byte{},
		treeOrder:    [][]byte{},
		storedHashes: make(map[string]struct{}),
		storedNodes:  make(map[string]struct{}),
		storedTrees:  make(map[string]struct{}),
		digestFS:     digestFS,
		options:      options,
	}
}

func (c *CAS[HM]) writeHeaderAndData(hdr *tar.Header, data io.Reader) error {
	// Create a tar entry with header and data combined
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if hdr.Typeflag == tar.TypeReg {
		data = io.LimitReader(data, hdr.Size)
		multireader := io.MultiReader(bytes.NewBuffer(buf.Bytes()), data)
		paddedreader := &paddedReader{
			Reader:  multireader,
			padSize: 512, // tar block size
		}
		return c.tarAppender.AppendTar(paddedreader)
	} else {
		if err := tw.Flush(); err != nil {
			return err
		}
		return c.tarAppender.AppendTar(bytes.NewReader(buf.Bytes()))
	}
}

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

// Close closes the tar archive by flushing the padding, and optionally writing the footer.
// If the current file (from a prior call to Writer.WriteHeader) is not fully written,
// then this returns an error.
func (c *CAS[HM]) Close() error {
	if c.closed {
		return nil
	}

	c.closed = true
	for _, hdr := range c.deferredFiles {
		if err := c.writeHeaderOrDefer(hdr, nil); err != nil {
			return fmt.Errorf("error writing deferred header: %w", err)
		}
	}

	return nil
}

func (c *CAS[HM]) WriteHeader(hdr *tar.Header) error {
	if hdr.Typeflag == tar.TypeReg {
		return errors.New("WriteHeader called with regular file header, use WriteRegular instead")
	}

	return c.writeHeaderOrDefer(hdr, nil)
}

func (c *CAS[HM]) WriteRegular(hdr *tar.Header, r io.Reader) error {
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("WriteRegular called with non-regular header: %s", hdr.Name)
	}
	return c.writeHeaderOrDefer(hdr, r)
}

func (c *CAS[HM]) WriteRegularFromPath(hdr *tar.Header, filePath string) error {
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("WriteRegularFromPath called with non-regular header: %s", hdr.Name)
	}

	df, err := c.digestFS.OpenFile(filePath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := df.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing file: %v\n", closeErr)
		}
	}()

	return c.writeHeaderOrDefer(hdr, df)
}

func (c *CAS[HM]) WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error {
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("WriteRegularFromPathDeduplicated called with non-regular header: %s", hdr.Name)
	}

	df, err := c.digestFS.OpenFile(filePath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := df.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing file: %v\n", closeErr)
		}
	}()

	// Get digest and size efficiently from digestFS
	hash, err := df.Digest()
	if err != nil {
		return err
	}

	size := df.Size()
	if size != hdr.Size {
		return fmt.Errorf("expected file of size %d, got %d", hdr.Size, size)
	}

	// Reset to start for reading
	if _, err := df.Seek(0, io.SeekStart); err != nil {
		return err
	}

	var linkPath string
	var storeErr error

	if isBlobTarHeader(hdr) {
		linkPath, storeErr = c.StoreKnownHashAndSize(df, hash, size)
	} else {
		linkPath, storeErr = c.StoreNodeKnownHash(df, hdr, hash)
	}
	if storeErr != nil {
		return storeErr
	}

	if linkPath == hdr.Name {
		// If we were writing to the CAS object itself,
		// we don't need to write a hardlink.
		return nil
	}

	header := cloneTarHeader(hdr)
	header.Typeflag = tar.TypeLink
	header.Linkname = linkPath
	header.Size = 0
	return c.writeHeaderOrDefer(&header, nil)
}

func (c *CAS[HM]) WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error {
	if hdr.Typeflag != tar.TypeReg {
		return fmt.Errorf("WriteRegular called with non-regular header: %s", hdr.Name)
	}

	var linkPath string
	var sz int64
	var storeErr error

	if isBlobTarHeader(hdr) {
		linkPath, _, sz, storeErr = c.Store(r)
	} else {
		linkPath, _, sz, storeErr = c.StoreNode(r, hdr)
	}
	if storeErr != nil {
		return storeErr
	}
	if sz != hdr.Size {
		return fmt.Errorf("expected file of size %d, got %d", hdr.Size, sz)
	}
	if linkPath == hdr.Name {
		// If we were writing to the CAS object itself,
		// we don't need to write a hardlink.
		return nil
	}
	header := cloneTarHeader(hdr)
	header.Typeflag = tar.TypeLink
	header.Linkname = linkPath
	header.Size = 0
	return c.writeHeaderOrDefer(&header, nil)
}

func (c *CAS[HM]) Store(r io.Reader) (string, []byte, int64, error) {
	var helper HM
	var buf bytes.Buffer
	h := helper.New()
	n, err := io.Copy(io.MultiWriter(h, &buf), r)
	if err != nil {
		return "", nil, n, err
	}
	hash := h.Sum(nil)
	contentPath, err := c.StoreKnownHashAndSize(&buf, hash, n)
	return contentPath, hash, n, err
}

func (c *CAS[HM]) StoreKnownHashAndSize(r io.Reader, hash []byte, size int64) (string, error) {
	contentName := casPath("blob", hash)

	if _, exists := c.storedHashes[string(hash)]; exists {
		return contentName, nil
	}

	header := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     contentName,
		Size:     size,
		Mode:     0o755,
	}

	if err := c.writeHeaderAndData(header, r); err != nil {
		return "", err
	}

	c.storedHashes[string(hash)] = struct{}{}
	c.hashOrder = append(c.hashOrder, hash)

	return contentName, nil
}

func (c *CAS[HM]) StoreNode(r io.Reader, hdr *tar.Header) (linkPath string, blobHash []byte, size int64, err error) {
	// TODO: cache content hashing in vfs
	var helper HM
	var buf bytes.Buffer
	h := helper.New()
	n, err := io.Copy(io.MultiWriter(h, &buf), r)
	if err != nil {
		return "", nil, n, err
	}
	blobHash = h.Sum(nil)
	linkPath, err = c.StoreNodeKnownHash(&buf, hdr, blobHash)
	return linkPath, blobHash, n, err
}

func (c *CAS[HM]) StoreNodeKnownHash(r io.Reader, hdr *tar.Header, blobHash []byte) (linkPath string, err error) {
	var helper HM

	// nodes are like blobs (regular files with content),
	// but they also have metadata (like permissions, owner, group, mtime, xattrs, etc.)
	// we need to account for that in the hash

	if hdr.Typeflag != tar.TypeReg || strings.HasSuffix(hdr.Name, "/") {
		// only regular files can be stored as nodes
		// other kinds cannot be targets of hardlinks
		return "", fmt.Errorf("invalid node header: %s", hdr.Name)
	}

	// create a normalized version of the header
	recordedTarHeader := cloneTarHeader(hdr)
	// we explicitly leave the name empty for hashing
	// so that files in different locations can hardlink the same
	// CAS entry.
	recordedTarHeader.Name = ""
	normalizeTarHeader(&recordedTarHeader)

	hasher := helper.New()
	hashTarHeader(hasher, recordedTarHeader)
	hasher.Write(blobHash)
	nodeHash := hasher.Sum(nil)

	linkPath = casPath("node", nodeHash)

	if _, exists := c.storedNodes[string(nodeHash)]; exists {
		return linkPath, nil
	}

	recordedTarHeader.Name = linkPath

	if err := c.writeHeaderAndData(&recordedTarHeader, r); err != nil {
		return linkPath, err
	}

	c.storedNodes[string(nodeHash)] = struct{}{}
	c.nodeOrder = append(c.nodeOrder, nodeHash)
	return linkPath, nil
}

func (c *CAS[HM]) StoreFileFromPath(filePath string) (string, []byte, int64, error) {
	df, err := c.digestFS.OpenFile(filePath)
	if err != nil {
		return "", nil, 0, err
	}
	defer func() {
		if closeErr := df.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "Error closing file: %v\n", closeErr)
		}
	}()

	hash, err := df.Digest()
	if err != nil {
		return "", nil, 0, err
	}

	size := df.Size()
	if _, err := df.Seek(0, io.SeekStart); err != nil {
		return "", nil, 0, err
	}

	contentPath, err := c.StoreKnownHashAndSize(df, hash, size)
	return contentPath, hash, size, err
}

func (c *CAS[HM]) StoreNodeFromPath(filePath string, hdr *tar.Header) (linkPath string, blobHash []byte, size int64, err error) {
	df, err := c.digestFS.OpenFile(filePath)
	if err != nil {
		return "", nil, 0, err
	}
	defer func() { _ = df.Close() }()

	hash, err := df.Digest()
	if err != nil {
		return "", nil, 0, err
	}

	size = df.Size()
	if _, err := df.Seek(0, io.SeekStart); err != nil {
		return "", nil, 0, err
	}

	linkPath, err = c.StoreNodeKnownHash(df, hdr, hash)
	return linkPath, hash, size, err
}

func (c *CAS[HM]) StoreTree(fsys fs.FS) (linkPath string, err error) {
	var hashMaker HM
	treeHasher := merkle.NewTreeHasher(fsys, hashMaker.New)
	rootHash, err := treeHasher.Build()
	if err != nil {
		return "", fmt.Errorf("calculating tree hash before storing tree artifact in tar: %w", err)
	}
	return c.StoreTreeKnownHash(fsys, rootHash)
}

func (c *CAS[HM]) StoreTreeKnownHash(fsys fs.FS, treeHash []byte) (linkPath string, err error) {
	// Every regular file in the tree is a CAS object, so we need to store it,
	// along with a hardlink to the CAS object.
	// For now, we don't support any special metadata for tree artifacts and disallow empty directories,
	// so we can get away with storing a single directory entry (for the root directory of the tree).
	treeBase := casPath("tree", treeHash)
	if _, exists := c.storedTrees[string(treeHash)]; exists {
		return treeBase, nil
	}

	header := &tar.Header{
		Typeflag: tar.TypeDir,
		Name:     treeBase,
		Mode:     0o755,
	}
	if err := c.writeHeaderAndData(header, nil); err != nil {
		return treeBase, err
	}

	// Store the tree children in the tar file.
	if err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walking directory %s: %w", p, err)
		}
		if !d.Type().IsRegular() {
			// Skip non-regular files
			return nil
		}
		f, err := fsys.Open(p)
		if err != nil {
			return fmt.Errorf("opening file %s: %w", p, err)
		}
		defer func() {
			if closeErr := f.Close(); closeErr != nil {
				fmt.Fprintf(os.Stderr, "Error closing file: %v\n", closeErr)
			}
		}()
		linkName, _, _, err := c.Store(f)
		if err != nil {
			return fmt.Errorf("storing file %s: %w", p, err)
		}

		header := &tar.Header{
			Typeflag: tar.TypeLink,
			Name:     path.Join(treeBase, p),
			Linkname: linkName,
			Mode:     0o755,
		}
		if err := c.writeHeaderAndData(header, nil); err != nil {
			return fmt.Errorf("writing link for %s: %w", p, err)
		}
		return nil
	}); err != nil {
		return treeBase, fmt.Errorf("storing tree artifact %x in tar: %w", treeHash, err)
	}

	c.storedTrees[string(treeHash)] = struct{}{}
	c.treeOrder = append(c.treeOrder, treeHash)
	return treeBase, nil
}

func (c *CAS[HM]) writeHeaderOrDefer(hdr *tar.Header, data io.Reader) error {
	if hdr.Typeflag != tar.TypeReg && c.structure == CASFirst && !c.closed {
		// Defer writing the header for non-regular files
		// until Close() is called.
		c.deferredFiles = append(c.deferredFiles, hdr)
		return nil
	}

	if c.writeHeaderCallback != nil && callbackModeFromTarType(hdr)&c.writeHeaderCallbackFilter != 0 {
		if err := c.writeHeaderCallback(hdr); err != nil {
			return fmt.Errorf("WriteHeader callback error: %w", err)
		}
	}

	if hdr.Typeflag != tar.TypeReg && c.structure == CASOnly {
		// Skip writing the header for non-regular files
		// if the structure should only contain regular files (CAS objects).
		return nil
	}

	// We are either writing a regular files (CAS object)
	// Or are in intertwined mode (CAS and non-CAS objects are mixed together as they are written)
	// Or we are in CASFirst mode and we are about to close the tar (so we need to write the deferred files)
	return c.writeHeaderAndData(hdr, data)
}

func casPath(blobKind string, hash []byte) string {
	return fmt.Sprintf(".cas/%s/%x", blobKind, hash)
}

func callbackModeFromTarType(hdr *tar.Header) WriteHeaderCallbackFilter {
	switch hdr.Typeflag {
	case tar.TypeReg:
		return WriteHeaderCallbackRegular
	case tar.TypeDir:
		return WriteHeaderCallbackDir
	case tar.TypeLink:
		return WriteHeaderCallbackLink
	case tar.TypeSymlink:
		return WriteHeaderCallbackSymlink
	}
	return 0
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
	return func(yield func([]byte, error) bool) {
		for _, hash := range e.hashOrder {
			if !yield(hash, nil) {
				return
			}
		}
	}
}

func (e *exporterState) NodeHashes() iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		for _, hash := range e.nodeOrder {
			if !yield(hash, nil) {
				return
			}
		}
	}
}

func (e *exporterState) TreeHashes() iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		for _, hash := range e.treeOrder {
			if !yield(hash, nil) {
				return
			}
		}
	}
}

type paddedReader struct {
	io.Reader
	n       int
	eof     bool
	padSize int
}

func (p *paddedReader) Read(b []byte) (int, error) {
	if p.eof || p.padSize <= 0 {
		return p.Reader.Read(b)
	}

	n, err := p.Reader.Read(b)
	p.n += n
	if err == io.EOF {
		p.eof = true
		blockFill := p.n % p.padSize
		var padding []byte
		if blockFill == 0 {
			padding = nil
		} else {
			padding = make([]byte, p.padSize-blockFill)
		}
		p.Reader = bytes.NewReader(padding)
		return n, nil
	}

	return n, err
}
