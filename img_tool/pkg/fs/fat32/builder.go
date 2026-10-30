package fat32

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
)

// source provides the content of a regular file.
type source interface {
	open() (io.ReadCloser, error)
	// retain registers one more user of the source, for a file that is copied
	// (a hard link). Every user calls release once.
	retain()
	// release frees what the source holds once no user is left.
	release()
}

type memSource []byte

func (m memSource) open() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m)), nil
}

func (memSource) retain()  {}
func (memSource) release() {}

type pathSource string

func (p pathSource) open() (io.ReadCloser, error) {
	return os.Open(string(p))
}

func (pathSource) retain()  {}
func (pathSource) release() {}

// spoolSource is content that was copied to a temporary file of the Builder.
type spoolSource struct {
	b    *Builder
	path string
	// users is shared by the copies of a hard-linked file.
	users *int
}

func (s spoolSource) open() (io.ReadCloser, error) { return os.Open(s.path) }

func (s spoolSource) retain() { *s.users++ }

func (s spoolSource) release() {
	if *s.users--; *s.users > 0 {
		return
	}
	if _, ok := s.b.spools[s.path]; !ok {
		return
	}
	delete(s.b.spools, s.path)
	os.Remove(s.path)
}

// node is a file or directory in the tree under construction.
type node struct {
	seq  int
	kind kind

	mode     uint32
	atime    time.Time
	mtime    time.Time
	size     int64
	src      source
	children map[string]*node
	// digest is the SHA-256 of the content of a regular file, if known.
	digest []byte
	// folded maps the folded name of every child to its name, to find names
	// that differ only in case.
	folded map[string]string

	planned planned
}

// Builder accumulates a file tree and writes it as a FAT32 image.
//
// A Builder is not safe for concurrent use. It has the tar-writing methods of
// the ext4 builder, so the layer recorder can drive it the same way. An entry
// that is added again replaces the earlier one; adding a directory again
// updates its metadata and keeps its children. FAT cannot share the data of
// files, so the Deduplicated methods store content like the plain ones.
type Builder struct {
	opts    Options
	root    *node
	nextSeq int
	// spools are the temporary files that hold streamed content.
	spools map[string]struct{}
	tars   tarCursors
	// pathDigests remembers the digest of files read from a path, so a file
	// that is added several times is only hashed once.
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
	return b, nil
}

func (b *Builder) newNode(k kind) *node {
	n := &node{seq: b.nextSeq, kind: k}
	b.nextSeq++
	if k == kindDir {
		n.children = make(map[string]*node)
		n.folded = make(map[string]string)
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

// drop releases the content of a node that is gone from the tree, and of the
// subtree it held.
func (b *Builder) drop(n *node) {
	if n.src != nil {
		n.src.release()
	}
	for _, child := range n.children {
		b.drop(child)
	}
}

// WriteHeader adds an entry without content: a directory, a hard link or an
// empty regular file. Symbolic links, device nodes and FIFOs cannot be stored
// in a FAT filesystem and are an error.
func (b *Builder) WriteHeader(hdr *tar.Header) error {
	switch hdr.Typeflag {
	case tar.TypeReg, '\x00', tar.TypeCont, tar.TypeGNUSparse:
		if hdr.Size != 0 {
			return fmt.Errorf("fat32: %s: regular file of %d bytes needs content, use WriteRegular", hdr.Name, hdr.Size)
		}
		return b.add(hdr, memSource(nil), nil)
	case tar.TypeXGlobalHeader:
		return nil
	}
	return b.add(hdr, nil, nil)
}

// WriteRegular adds a regular file whose hdr.Size bytes of content are read
// from r. The content is kept in memory, or in a temporary file when it
// exceeds [Options.SpoolThreshold], until Build.
func (b *Builder) WriteRegular(hdr *tar.Header, r io.Reader) error {
	return b.writeRegular(hdr, r, nil)
}

func (b *Builder) writeRegular(hdr *tar.Header, r io.Reader, digest []byte) error {
	if err := checkRegular(hdr); err != nil {
		return err
	}
	src, err := b.spool(r, hdr.Size)
	if err != nil {
		return fmt.Errorf("fat32: %s: %w", hdr.Name, err)
	}
	return b.add(hdr, src, digest)
}

func checkRegular(hdr *tar.Header) error {
	if !isRegular(hdr.Typeflag) {
		return fmt.Errorf("fat32: %s: type %q is not a regular file", hdr.Name, hdr.Typeflag)
	}
	if hdr.Size < 0 || hdr.Size > maxFileSize {
		return fmt.Errorf("fat32: %s: size %d is not between 0 and 4 GiB - 1", hdr.Name, hdr.Size)
	}
	return nil
}

// WriteRegularDeduplicated is like WriteRegular: FAT has no hard links to
// merge identical files with.
func (b *Builder) WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error {
	return b.WriteRegular(hdr, r)
}

// WriteRegularDeduplicatedKnownHash is like WriteRegular for a file whose
// SHA-256 digest the caller already knows. Other digests are ignored. With
// [Options.DigestFiles] the digest is trusted instead of computed again.
func (b *Builder) WriteRegularDeduplicatedKnownHash(hdr *tar.Header, r io.Reader, blobHash []byte) error {
	return b.writeRegular(hdr, r, sha256Digest(blobHash))
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
	if err := checkRegular(hdr); err != nil {
		return err
	}
	return b.add(hdr, pathSource(filePath), nil)
}

// WriteRegularFromPathDeduplicated is like WriteRegularFromPath.
func (b *Builder) WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error {
	return b.WriteRegularFromPath(hdr, filePath)
}

// ImportTar adds every entry of an uncompressed tar stream.
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
			err = b.WriteRegular(hdr, tr)
		} else {
			err = b.WriteHeader(hdr)
		}
		if err != nil {
			return err
		}
	}
}

// AddFS adds the files of fsys below the directory prefix of the image (the
// root of the image if prefix is empty). Only the read-only flag and the
// default timestamps are kept, which keeps the image reproducible. Symbolic
// links and other special files are an error.
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
		hdr := &tar.Header{Name: path.Join(prefix, p), Mode: int64(mode.Perm())}
		switch {
		case mode.IsDir():
			hdr.Typeflag = tar.TypeDir
			return b.WriteHeader(hdr)
		case mode.IsRegular():
			f, err := fsys.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			hdr.Typeflag = tar.TypeReg
			hdr.Size = info.Size()
			return b.WriteRegular(hdr, f)
		}
		return fmt.Errorf("fat32: %s: FAT cannot store files of type %v", p, mode.Type())
	})
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

	f, err := os.CreateTemp(b.opts.SpoolDir, "fat32-spool-*")
	if err != nil {
		return nil, err
	}
	b.spools[f.Name()] = struct{}{}
	spooled := spoolSource{b: b, path: f.Name(), users: new(int)}
	*spooled.users = 1
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

// splitPath turns the name of a tar entry into validated path components.
// The root has no components.
func splitPath(name string) ([]string, error) {
	var parts []string
	for _, c := range strings.Split(name, "/") {
		switch c {
		case "", ".":
		case "..":
			return nil, fmt.Errorf("fat32: path %q escapes the root", name)
		default:
			if err := validateName(c); err != nil {
				return nil, fmt.Errorf("fat32: path %q: %w", name, err)
			}
			parts = append(parts, c)
		}
	}
	return parts, nil
}

// add inserts the entry described by hdr, with the given content for
// regular files.
func (b *Builder) add(hdr *tar.Header, src source, digest []byte) error {
	if b.built {
		return errors.New("fat32: image already built")
	}
	parts, err := splitPath(hdr.Name)
	if err != nil {
		return err
	}

	var k kind
	switch hdr.Typeflag {
	case tar.TypeReg, '\x00', tar.TypeCont, tar.TypeGNUSparse:
		k = kindReg
	case tar.TypeDir:
		k = kindDir
	case tar.TypeLink:
		return b.addHardLink(hdr, parts)
	case tar.TypeSymlink:
		return fmt.Errorf("fat32: %s: FAT cannot store symbolic links", hdr.Name)
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return fmt.Errorf("fat32: %s: FAT cannot store device nodes and FIFOs", hdr.Name)
	default:
		return fmt.Errorf("fat32: %s: unsupported tar entry type %q", hdr.Name, hdr.Typeflag)
	}

	// A node that never makes it into the tree must not keep its content.
	n := b.newNode(k)
	placed := false
	defer func() {
		if !placed && src != nil {
			src.release()
		}
	}()
	b.fill(n, hdr)
	if k == kindReg {
		n.size, n.src, n.digest = hdr.Size, src, digest
	}

	if len(parts) == 0 {
		if k != kindDir {
			return fmt.Errorf("fat32: %s: the root must be a directory", hdr.Name)
		}
		return nil // the root of FAT has no metadata
	}
	parent, err := b.parentDir(parts[:len(parts)-1])
	if err != nil {
		return fmt.Errorf("fat32: %s: %w", hdr.Name, err)
	}
	if placed, err = b.put(parent, parts[len(parts)-1], n); err != nil {
		return fmt.Errorf("fat32: %s: %w", hdr.Name, err)
	}
	return nil
}

// addHardLink adds a copy of the file the link points to.
func (b *Builder) addHardLink(hdr *tar.Header, parts []string) error {
	if len(parts) == 0 {
		return fmt.Errorf("fat32: %s: cannot replace the root with a hard link", hdr.Name)
	}
	targetParts, err := splitPath(hdr.Linkname)
	if err != nil {
		return err
	}
	target := b.root
	for _, c := range targetParts {
		next := b.lookup(target, c)
		if target.kind != kindDir || next == nil {
			return fmt.Errorf("fat32: %s: hard link target %q does not exist", hdr.Name, hdr.Linkname)
		}
		target = next
	}
	if target.kind == kindDir {
		return fmt.Errorf("fat32: %s: hard link target %q is a directory", hdr.Name, hdr.Linkname)
	}
	parent, err := b.parentDir(parts[:len(parts)-1])
	if err != nil {
		return fmt.Errorf("fat32: %s: %w", hdr.Name, err)
	}
	n := b.newNode(kindReg)
	b.fill(n, hdr)
	n.size, n.src, n.digest = target.size, target.src, target.digest // FAT has no hard links: a copy
	n.src.retain()
	placed, err := b.put(parent, parts[len(parts)-1], n)
	if !placed {
		n.src.release()
	}
	if err != nil {
		return fmt.Errorf("fat32: %s: %w", hdr.Name, err)
	}
	return nil
}

// lookup finds the child of dir called name, ignoring case.
func (b *Builder) lookup(dir *node, name string) *node {
	if dir.kind != kindDir {
		return nil
	}
	if exact, ok := dir.folded[foldName(name)]; ok {
		return dir.children[exact]
	}
	return nil
}

// parentDir returns the directory at the given path, creating missing
// directories on the way.
func (b *Builder) parentDir(parts []string) (*node, error) {
	dir := b.root
	for _, c := range parts {
		child := b.lookup(dir, c)
		if child == nil {
			child = b.newNode(kindDir)
			child.mode = 0o755
			if _, err := b.put(dir, c, child); err != nil {
				return nil, err
			}
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
// reports false then, as n is not part of the tree. Names are case
// insensitive: a name that differs only in case from another is an error.
func (b *Builder) put(dir *node, name string, n *node) (bool, error) {
	fold := foldName(name)
	if exact, ok := dir.folded[fold]; ok {
		if exact != name {
			return false, fmt.Errorf("%q and %q differ only in case, which FAT cannot tell apart", name, exact)
		}
		old := dir.children[name]
		if old.kind == kindDir && n.kind == kindDir {
			old.mode, old.atime, old.mtime = n.mode, n.atime, n.mtime
			return false, nil
		}
		b.drop(old)
	}
	dir.children[name] = n
	dir.folded[fold] = name
	return true, nil
}

func (b *Builder) fill(n *node, hdr *tar.Header) {
	n.mode = uint32(hdr.Mode & 0o7777)
	n.mtime, n.atime = hdr.ModTime, hdr.AccessTime
	// An absent access time is the modification time, which in turn defaults.
	if n.atime.IsZero() {
		n.atime = n.mtime
	}
}
