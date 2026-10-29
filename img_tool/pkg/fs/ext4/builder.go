package ext4

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

type kind uint8

const (
	kindReg kind = iota
	kindDir
	kindSymlink
	kindChar
	kindBlock
	kindFifo
)

// source provides the content of a regular file.
type source interface {
	open() (io.ReadCloser, error)
	// release frees what the source holds once no entry of the tree
	// references it anymore.
	release()
}

type memSource []byte

func (m memSource) open() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m)), nil
}

func (memSource) release() {}

type pathSource string

func (p pathSource) open() (io.ReadCloser, error) {
	return os.Open(string(p))
}

func (pathSource) release() {}

// spoolSource is content that was copied to a temporary file of the Builder.
type spoolSource struct {
	b    *Builder
	path string
}

func (s spoolSource) open() (io.ReadCloser, error) { return os.Open(s.path) }

func (s spoolSource) release() {
	if _, ok := s.b.spools[s.path]; !ok {
		return
	}
	delete(s.b.spools, s.path)
	os.Remove(s.path)
}

// node is a file, directory or other object in the tree under construction.
// The same node is referenced by several directory entries for hard links.
type node struct {
	seq  int
	kind kind

	mode     uint32 // permission bits and setuid, setgid and sticky
	uid, gid uint32
	atime    time.Time
	ctime    time.Time
	mtime    time.Time
	size     int64
	src      source
	target   string // symlink target
	major    uint32
	minor    uint32
	xattrs   []xattr
	children map[string]*node

	// refs is the number of directory entries that reference the node. When
	// it drops to zero the node is gone from the tree and releases its content.
	refs int

	// dedup marks a regular file that may be merged with identical files
	// (see Options.Deduplicate); digest is the SHA-256 of its content if known.
	dedup  bool
	digest []byte

	planned planned
}

// Builder accumulates a file tree and writes it as an ext4 image.
//
// A Builder is not safe for concurrent use. It satisfies the tar-writing
// half of the interface the layer recorder drives: [Builder.WriteHeader],
// [Builder.WriteRegular], [Builder.WriteRegularFromPath] and their
// deduplicating variants, which store content just like the plain ones.
// An entry that is added again replaces the earlier one; adding a directory
// again updates its metadata and keeps its children.
type Builder struct {
	opts    Options
	root    *node
	nextSeq int
	// spools are the temporary files that hold streamed content.
	spools map[string]struct{}
	tars   tarCursors
	// pathDigests remembers the digest of files read from a path, so a file
	// that is added several times (or hashed for deduplication and again for
	// the image) is only hashed once.
	pathDigests map[pathKey][]byte
	built       bool
}

type pathKey struct {
	path string
	size int64
}

// NewBuilder returns a Builder for an empty filesystem.
func NewBuilder(opts Options) (*Builder, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	b := &Builder{opts: opts, spools: map[string]struct{}{}, pathDigests: map[pathKey][]byte{}}
	b.root = b.newNode(kindDir)
	b.root.mode = 0o755
	b.root.refs = 1
	if !opts.OmitLostAndFound {
		lf := b.newNode(kindDir)
		lf.mode = 0o700
		b.setChild(b.root, "lost+found", lf)
	}
	return b, nil
}

func (b *Builder) newNode(k kind) *node {
	n := &node{seq: b.nextSeq, kind: k}
	b.nextSeq++
	if k == kindDir {
		n.children = make(map[string]*node)
	}
	return n
}

// Close releases the temporary files of the Builder. It does not write
// anything.
func (b *Builder) Close() error {
	var errs []error
	for p := range b.spools {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	b.spools = map[string]struct{}{}
	errs = append(errs, b.tars.close())
	return errors.Join(errs...)
}

// setChild places n at name in dir, replacing the entry that is there.
func (b *Builder) setChild(dir *node, name string, n *node) {
	old := dir.children[name]
	dir.children[name] = n
	n.refs++
	if old != nil {
		b.unref(old)
	}
}

// unref drops a reference to n. A node nothing references anymore is gone, and
// so is the content (and the subtree) it held.
func (b *Builder) unref(n *node) {
	n.refs--
	if n.refs > 0 {
		return
	}
	if n.src != nil {
		n.src.release()
	}
	for _, child := range n.children {
		b.unref(child)
	}
}

// WriteHeader adds an entry without content: a directory, symlink, hard
// link, device node, fifo or empty regular file.
func (b *Builder) WriteHeader(hdr *tar.Header) error {
	switch hdr.Typeflag {
	case tar.TypeReg, '\x00', tar.TypeCont, tar.TypeGNUSparse:
		if hdr.Size != 0 {
			return fmt.Errorf("ext4: %s: regular file of %d bytes needs content, use WriteRegular", hdr.Name, hdr.Size)
		}
		return b.add(hdr, memSource(nil), false, nil)
	case tar.TypeXGlobalHeader:
		return nil
	}
	return b.add(hdr, nil, false, nil)
}

// WriteRegular adds a regular file whose hdr.Size bytes of content are read
// from r. The content is kept in memory, or in a temporary file when it
// exceeds [Options.SpoolThreshold], until Build.
func (b *Builder) WriteRegular(hdr *tar.Header, r io.Reader) error {
	return b.writeRegular(hdr, r, false, nil)
}

func (b *Builder) writeRegular(hdr *tar.Header, r io.Reader, dedup bool, digest []byte) error {
	if !isRegular(hdr.Typeflag) {
		return fmt.Errorf("ext4: %s: type %q is not a regular file", hdr.Name, hdr.Typeflag)
	}
	if hdr.Size < 0 {
		return fmt.Errorf("ext4: %s: negative size", hdr.Name)
	}
	src, err := b.spool(r, hdr.Size)
	if err != nil {
		return fmt.Errorf("ext4: %s: %w", hdr.Name, err)
	}
	return b.add(hdr, src, dedup, digest)
}

// WriteRegularDeduplicated is like WriteRegular, but with [Options.Deduplicate]
// the file is merged with other files that have the same content and
// metadata: they become hard links to a single copy.
func (b *Builder) WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error {
	return b.writeRegular(hdr, r, b.opts.Deduplicate, nil)
}

// WriteRegularDeduplicatedKnownHash is like WriteRegularDeduplicated for a
// file whose SHA-256 digest the caller already knows. Other digests are
// ignored. The digest is trusted: if it is wrong, files with different
// content may be merged.
func (b *Builder) WriteRegularDeduplicatedKnownHash(hdr *tar.Header, r io.Reader, blobHash []byte) error {
	return b.writeRegular(hdr, r, b.opts.Deduplicate, sha256Digest(blobHash))
}

func sha256Digest(d []byte) []byte {
	if len(d) != sha256.Size {
		return nil
	}
	return append([]byte(nil), d...)
}

// WriteRegularFromPath adds a regular file whose hdr.Size bytes of content
// are read from the file at filePath during Build. The file must not change
// before then.
func (b *Builder) WriteRegularFromPath(hdr *tar.Header, filePath string) error {
	return b.writeRegularFromPath(hdr, filePath, false)
}

func (b *Builder) writeRegularFromPath(hdr *tar.Header, filePath string, dedup bool) error {
	if !isRegular(hdr.Typeflag) {
		return fmt.Errorf("ext4: %s: type %q is not a regular file", hdr.Name, hdr.Typeflag)
	}
	if hdr.Size < 0 {
		return fmt.Errorf("ext4: %s: negative size", hdr.Name)
	}
	return b.add(hdr, pathSource(filePath), dedup, nil)
}

// WriteRegularFromPathDeduplicated is like WriteRegularFromPath, but with
// [Options.Deduplicate] the file is merged with identical files, like
// [Builder.WriteRegularDeduplicated].
func (b *Builder) WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error {
	return b.writeRegularFromPath(hdr, filePath, b.opts.Deduplicate)
}

// ImportTar adds every entry of an uncompressed tar stream. Regular files are
// added like by [Builder.WriteRegularDeduplicated].
func (b *Builder) ImportTar(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if isRegular(hdr.Typeflag) {
			err = b.WriteRegularDeduplicated(hdr, tr)
		} else {
			err = b.WriteHeader(hdr)
		}
		if err != nil {
			return err
		}
	}
}

// AddFS adds the files of fsys below the directory prefix of the image (the
// root of the image if prefix is empty). Regular files are added like by
// [Builder.WriteRegularDeduplicated]. Only the permission bits of the
// files are kept: ownership is root and timestamps are the default, which
// keeps the image reproducible.
func (b *Builder) AddFS(fsys fs.FS, prefix string) error {
	return fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		hdr := &tar.Header{Name: path.Join(prefix, p), Mode: int64(modeBits(mode))}
		switch {
		case mode.IsDir():
			hdr.Typeflag = tar.TypeDir
			return b.WriteHeader(hdr)
		case mode&fs.ModeSymlink != 0:
			link, err := fs.ReadLink(fsys, p)
			if err != nil {
				return err
			}
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = link
			return b.WriteHeader(hdr)
		case mode.IsRegular():
			f, err := fsys.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			hdr.Typeflag = tar.TypeReg
			hdr.Size = info.Size()
			return b.WriteRegularDeduplicated(hdr, f)
		}
		return fmt.Errorf("ext4: %s: unsupported file type %v", p, mode.Type())
	})
}

func modeBits(m fs.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

func isRegular(t byte) bool {
	switch t {
	case tar.TypeReg, '\x00', tar.TypeCont, tar.TypeGNUSparse:
		return true
	}
	return false
}

// spool keeps size bytes of r for later.
func (b *Builder) spool(r io.Reader, size int64) (source, error) {
	if size == 0 {
		return memSource(nil), nil
	}
	if r == nil {
		return nil, errors.New("missing content")
	}
	lr := io.LimitReader(r, size)
	inMemory := min(size, b.opts.SpoolThreshold)
	mem := make([]byte, inMemory)
	if _, err := io.ReadFull(lr, mem); err != nil {
		return nil, fmt.Errorf("reading content: %w", unexpected(err))
	}
	if inMemory == size {
		return memSource(mem), nil
	}

	dir := b.opts.SpoolDir
	f, err := os.CreateTemp(dir, "ext4-spool-*")
	if err != nil {
		return nil, err
	}
	b.spools[f.Name()] = struct{}{}
	spooled := spoolSource{b: b, path: f.Name()}
	if _, err := f.Write(mem); err != nil {
		f.Close()
		spooled.release()
		return nil, err
	}
	n, err := io.Copy(f, lr)
	if err == nil && n != size-inMemory {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		f.Close()
		spooled.release()
		return nil, fmt.Errorf("reading content: %w", err)
	}
	if err := f.Close(); err != nil {
		spooled.release()
		return nil, err
	}
	return spooled, nil
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// splitPath turns the name of a tar entry into path components. The root
// has no components.
func splitPath(name string) ([]string, error) {
	var parts []string
	for _, c := range strings.Split(name, "/") {
		switch c {
		case "", ".":
		case "..":
			return nil, fmt.Errorf("ext4: path %q escapes the root", name)
		default:
			if len(c) > maxNameLen {
				return nil, fmt.Errorf("ext4: path component %q is longer than %d bytes", c, maxNameLen)
			}
			parts = append(parts, c)
		}
	}
	return parts, nil
}

// add inserts the entry described by hdr, with the given content for
// regular files.
func (b *Builder) add(hdr *tar.Header, src source, dedup bool, digest []byte) error {
	if b.built {
		return errors.New("ext4: image already built")
	}
	parts, err := splitPath(hdr.Name)
	if err != nil {
		return err
	}

	if hdr.Typeflag == tar.TypeLink {
		return b.addHardLink(hdr, parts)
	}

	k, err := kindOf(hdr.Typeflag)
	if err != nil {
		return fmt.Errorf("ext4: %s: %w", hdr.Name, err)
	}
	// A node that never makes it into the tree must not keep its content.
	n := b.newNode(k)
	placed := false
	defer func() {
		if !placed && src != nil {
			src.release()
		}
	}()
	if err := b.fill(n, hdr, src); err != nil {
		return fmt.Errorf("ext4: %s: %w", hdr.Name, err)
	}
	n.dedup, n.digest = dedup && k == kindReg, digest

	if len(parts) == 0 {
		if k != kindDir {
			return fmt.Errorf("ext4: %s: the root must be a directory", hdr.Name)
		}
		adoptMetadata(b.root, n)
		return nil
	}
	parent, err := b.parentDir(parts[:len(parts)-1])
	if err != nil {
		return fmt.Errorf("ext4: %s: %w", hdr.Name, err)
	}
	placed = b.put(parent, parts[len(parts)-1], n)
	return nil
}

func (b *Builder) addHardLink(hdr *tar.Header, parts []string) error {
	if len(parts) == 0 {
		return fmt.Errorf("ext4: %s: cannot replace the root with a hard link", hdr.Name)
	}
	targetParts, err := splitPath(hdr.Linkname)
	if err != nil {
		return err
	}
	target := b.root
	for _, c := range targetParts {
		if target.kind != kindDir || target.children[c] == nil {
			return fmt.Errorf("ext4: %s: hard link target %q does not exist", hdr.Name, hdr.Linkname)
		}
		target = target.children[c]
	}
	if target.kind == kindDir {
		return fmt.Errorf("ext4: %s: hard link target %q is a directory", hdr.Name, hdr.Linkname)
	}
	parent, err := b.parentDir(parts[:len(parts)-1])
	if err != nil {
		return fmt.Errorf("ext4: %s: %w", hdr.Name, err)
	}
	b.setChild(parent, parts[len(parts)-1], target)
	return nil
}

// parentDir returns the directory at the given path, creating missing
// directories on the way.
func (b *Builder) parentDir(parts []string) (*node, error) {
	dir := b.root
	for _, c := range parts {
		child := dir.children[c]
		if child == nil {
			child = b.newNode(kindDir)
			child.mode = 0o755
			b.setChild(dir, c, child)
		}
		if child.kind != kindDir {
			return nil, fmt.Errorf("%q is not a directory", c)
		}
		dir = child
	}
	return dir, nil
}

// put places n at name in dir, replacing what is there. A directory that
// replaces a directory takes over the metadata and keeps the children; put
// reports false then, as n is not part of the tree.
func (b *Builder) put(dir *node, name string, n *node) bool {
	if old := dir.children[name]; old != nil && old.kind == kindDir && n.kind == kindDir {
		adoptMetadata(old, n)
		return false
	}
	b.setChild(dir, name, n)
	return true
}

func adoptMetadata(dst, src *node) {
	dst.mode, dst.uid, dst.gid = src.mode, src.uid, src.gid
	dst.atime, dst.ctime, dst.mtime = src.atime, src.ctime, src.mtime
	dst.xattrs = src.xattrs
}

func kindOf(t byte) (kind, error) {
	switch t {
	case tar.TypeReg, '\x00', tar.TypeCont, tar.TypeGNUSparse:
		return kindReg, nil
	case tar.TypeDir:
		return kindDir, nil
	case tar.TypeSymlink:
		return kindSymlink, nil
	case tar.TypeChar:
		return kindChar, nil
	case tar.TypeBlock:
		return kindBlock, nil
	case tar.TypeFifo:
		return kindFifo, nil
	}
	return 0, fmt.Errorf("unsupported tar entry type %q", t)
}

func (b *Builder) fill(n *node, hdr *tar.Header, src source) error {
	if hdr.Uid < 0 || hdr.Gid < 0 || int64(uint32(hdr.Uid)) != int64(hdr.Uid) || int64(uint32(hdr.Gid)) != int64(hdr.Gid) {
		return fmt.Errorf("owner %d:%d does not fit in 32 bits", hdr.Uid, hdr.Gid)
	}
	n.mode = uint32(hdr.Mode) & 0o7777
	n.uid, n.gid = uint32(hdr.Uid), uint32(hdr.Gid)
	n.mtime, n.atime, n.ctime = hdr.ModTime, hdr.AccessTime, hdr.ChangeTime
	// Absent timestamps take the modification time, then the default.
	if n.atime.IsZero() {
		n.atime = n.mtime
	}
	if n.ctime.IsZero() {
		n.ctime = n.mtime
	}

	var err error
	if n.xattrs, err = xattrsFromHeader(hdr); err != nil {
		return err
	}

	switch n.kind {
	case kindReg:
		n.size = hdr.Size
		n.src = src
	case kindSymlink:
		if len(hdr.Linkname) >= b.opts.BlockSize {
			return fmt.Errorf("symlink target of %d bytes does not fit in a block", len(hdr.Linkname))
		}
		n.target = hdr.Linkname
	case kindChar, kindBlock:
		if hdr.Devmajor < 0 || hdr.Devminor < 0 || hdr.Devmajor >= 1<<12 || hdr.Devminor >= 1<<20 {
			return fmt.Errorf("device number %d:%d out of range", hdr.Devmajor, hdr.Devminor)
		}
		n.major, n.minor = uint32(hdr.Devmajor), uint32(hdr.Devminor)
	}
	return nil
}
