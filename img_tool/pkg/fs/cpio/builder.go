package cpio

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"
)

// source provides the content of a regular file.
type source interface {
	open() (io.ReadCloser, error)
}

type memSource []byte

func (m memSource) open() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(m)), nil
}

type pathSource string

func (p pathSource) open() (io.ReadCloser, error) {
	return os.Open(string(p))
}

// entry is a file in the tree under construction.
type entry struct {
	typ   uint32 // file type bits of the mode
	perm  uint32 // permission, setuid, setgid and sticky bits
	uid   int64
	gid   int64
	mtime int64
	size  int64
	src   source
	// target is the target of a symbolic link.
	target     string
	rmaj, rmin int64
	// inodeOwner identifies the inode: the entry itself, or the entry a hard
	// link was made to. nlink is only set on the owner.
	inodeOwner *entry
	inode      uint32
	nlink      uint32
}

// Builder accumulates a file tree and writes it as a cpio archive.
//
// A Builder is not safe for concurrent use. It has the tar-writing methods of
// the ext4 and FAT32 builders, so the layer recorder can drive it the same
// way. An entry that is added again replaces the earlier one.
type Builder struct {
	opts   Options
	files  map[string]*entry
	spools []string
	built  bool
}

// NewBuilder returns a Builder for an empty archive.
func NewBuilder(opts Options) (*Builder, error) {
	if opts.SpoolThreshold < 0 {
		return nil, errors.New("cpio: negative spool threshold")
	}
	if opts.SpoolThreshold == 0 {
		opts.SpoolThreshold = DefaultSpoolThreshold
	}
	return &Builder{opts: opts, files: make(map[string]*entry)}, nil
}

// Close releases the temporary files of the Builder. It does not write
// anything.
func (b *Builder) Close() error {
	var errs []error
	for _, p := range b.spools {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	b.spools = nil
	return errors.Join(errs...)
}

// WriteHeader adds an entry without content: a directory, a symbolic link, a
// device node, a FIFO, a hard link or an empty regular file.
func (b *Builder) WriteHeader(hdr *tar.Header) error {
	switch hdr.Typeflag {
	case tar.TypeReg, '\x00', tar.TypeCont, tar.TypeGNUSparse:
		if hdr.Size != 0 {
			return fmt.Errorf("cpio: %s: regular file of %d bytes needs content, use WriteRegular", hdr.Name, hdr.Size)
		}
		return b.add(hdr, memSource(nil))
	case tar.TypeXGlobalHeader:
		return nil
	}
	return b.add(hdr, nil)
}

// WriteRegular adds a regular file whose hdr.Size bytes of content are read
// from r. The content is kept in memory, or in a temporary file when it
// exceeds [Options.SpoolThreshold], until Build.
func (b *Builder) WriteRegular(hdr *tar.Header, r io.Reader) error {
	if !isRegular(hdr.Typeflag) {
		return fmt.Errorf("cpio: %s: type %q is not a regular file", hdr.Name, hdr.Typeflag)
	}
	if hdr.Size < 0 || hdr.Size > maxFileSize {
		return fmt.Errorf("cpio: %s: size %d is not between 0 and 4 GiB - 1", hdr.Name, hdr.Size)
	}
	src, err := b.spool(r, hdr.Size)
	if err != nil {
		return fmt.Errorf("cpio: %s: %w", hdr.Name, err)
	}
	return b.add(hdr, src)
}

// WriteRegularDeduplicated is like WriteRegular: identical files are only
// shared if the input says they are hard links.
func (b *Builder) WriteRegularDeduplicated(hdr *tar.Header, r io.Reader) error {
	return b.WriteRegular(hdr, r)
}

// WriteRegularDeduplicatedKnownHash is like WriteRegular and ignores the hash.
func (b *Builder) WriteRegularDeduplicatedKnownHash(hdr *tar.Header, r io.Reader, _ []byte) error {
	return b.WriteRegular(hdr, r)
}

// WriteRegularFromPath adds a regular file whose hdr.Size bytes of content
// are read from the file at filePath during Build. The file must not change
// before then.
func (b *Builder) WriteRegularFromPath(hdr *tar.Header, filePath string) error {
	if !isRegular(hdr.Typeflag) {
		return fmt.Errorf("cpio: %s: type %q is not a regular file", hdr.Name, hdr.Typeflag)
	}
	if hdr.Size < 0 || hdr.Size > maxFileSize {
		return fmt.Errorf("cpio: %s: size %d is not between 0 and 4 GiB - 1", hdr.Name, hdr.Size)
	}
	return b.add(hdr, pathSource(filePath))
}

// WriteRegularFromPathDeduplicated is like WriteRegularFromPath.
func (b *Builder) WriteRegularFromPathDeduplicated(hdr *tar.Header, filePath string) error {
	return b.WriteRegularFromPath(hdr, filePath)
}

func isRegular(t byte) bool {
	switch t {
	case tar.TypeReg, '\x00', tar.TypeCont, tar.TypeGNUSparse:
		return true
	}
	return false
}

func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
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

	f, err := os.CreateTemp(b.opts.SpoolDir, "cpio-spool-*")
	if err != nil {
		return nil, err
	}
	b.spools = append(b.spools, f.Name())
	if _, err := f.Write(mem); err != nil {
		f.Close()
		return nil, err
	}
	n, err := io.Copy(f, lr)
	if err == nil && n != size-inMemory {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("reading content: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return pathSource(f.Name()), nil
}

// cleanPath turns the name of a tar entry into the name of an archive
// member. The root is the empty string.
func cleanPath(name string) (string, error) {
	var parts []string
	for _, c := range strings.Split(name, "/") {
		switch c {
		case "", ".":
		case "..":
			return "", fmt.Errorf("cpio: path %q escapes the root", name)
		default:
			if strings.ContainsRune(c, 0) {
				return "", fmt.Errorf("cpio: path %q contains a NUL byte", name)
			}
			parts = append(parts, c)
		}
	}
	p := strings.Join(parts, "/")
	if len(p) > maxNameBytes {
		return "", fmt.Errorf("cpio: path %q is longer than %d bytes", name, maxNameBytes)
	}
	return p, nil
}

// add inserts the entry described by hdr, with the given content for
// regular files.
func (b *Builder) add(hdr *tar.Header, src source) error {
	if b.built {
		return errors.New("cpio: archive already built")
	}
	p, err := cleanPath(hdr.Name)
	if err != nil {
		return err
	}

	e := &entry{
		perm:  uint32(hdr.Mode) & 0o7777,
		uid:   int64(hdr.Uid),
		gid:   int64(hdr.Gid),
		mtime: hdr.ModTime.Unix(),
	}
	if hdr.ModTime.IsZero() {
		e.mtime = 0
	}
	e.inodeOwner = e

	switch hdr.Typeflag {
	case tar.TypeReg, '\x00', tar.TypeCont, tar.TypeGNUSparse:
		e.typ, e.size, e.src = modeReg, hdr.Size, src
	case tar.TypeDir:
		e.typ = modeDir
	case tar.TypeSymlink:
		e.typ, e.target = modeSymlink, hdr.Linkname
		e.size = int64(len(hdr.Linkname))
		if e.size > maxFileSize {
			return fmt.Errorf("cpio: %s: symbolic link target is too long", hdr.Name)
		}
	case tar.TypeChar:
		e.typ, e.rmaj, e.rmin = modeChar, hdr.Devmajor, hdr.Devminor
	case tar.TypeBlock:
		e.typ, e.rmaj, e.rmin = modeBlock, hdr.Devmajor, hdr.Devminor
	case tar.TypeFifo:
		e.typ = modeFifo
	case tar.TypeLink:
		if p == "" {
			return fmt.Errorf("cpio: %s: cannot replace the root with a hard link", hdr.Name)
		}
		return b.addHardLink(hdr, p, e)
	default:
		return fmt.Errorf("cpio: %s: unsupported tar entry type %q", hdr.Name, hdr.Typeflag)
	}
	for what, v := range map[string]int64{"uid": e.uid, "gid": e.gid, "device major": e.rmaj, "device minor": e.rmin} {
		if err := checkRange(what, hdr.Name, v); err != nil {
			return err
		}
	}
	if e.mtime < 0 || e.mtime > 1<<32-1 {
		return fmt.Errorf("cpio: %s: modification time %d does not fit in 32 bits", hdr.Name, e.mtime)
	}

	if p == "" {
		if e.typ != modeDir {
			return fmt.Errorf("cpio: %s: the root must be a directory", hdr.Name)
		}
		return nil // the root is not written
	}
	b.files[p] = e
	return nil
}

// addHardLink adds a member that shares the inode of an existing regular
// file. Hard links to anything else become copies.
func (b *Builder) addHardLink(hdr *tar.Header, p string, e *entry) error {
	tp, err := cleanPath(hdr.Linkname)
	if err != nil {
		return err
	}
	target, ok := b.files[tp]
	if !ok {
		return fmt.Errorf("cpio: %s: hard link target %q does not exist", hdr.Name, hdr.Linkname)
	}
	if target.typ == modeDir {
		return fmt.Errorf("cpio: %s: hard link target %q is a directory", hdr.Name, hdr.Linkname)
	}
	// The link keeps the metadata of the file it points to, like on a file
	// system where they are the same inode.
	cp := *target
	if target.typ == modeReg {
		cp.inodeOwner = target.inodeOwner
	} else {
		cp.inodeOwner = &cp
	}
	b.files[p] = &cp
	return nil
}

// Build writes the archive to w.
func (b *Builder) Build(w io.Writer) (*Info, error) {
	if b.built {
		return nil, errors.New("cpio: archive already built")
	}
	b.built = true

	// Parent directories nothing describes.
	for p := range b.files {
		for i := strings.LastIndexByte(p, '/'); i > 0; i = strings.LastIndexByte(p[:i], '/') {
			parent := p[:i]
			switch pe, ok := b.files[parent]; {
			case !ok:
				d := &entry{typ: modeDir, perm: 0o755}
				d.inodeOwner = d
				b.files[parent] = d
			case pe.typ != modeDir:
				return nil, fmt.Errorf("cpio: %s: parent %q is not a directory", p, parent)
			}
		}
	}

	names := make([]string, 0, len(b.files))
	for p := range b.files {
		names = append(names, p)
	}
	slices.Sort(names)

	// Inodes and link counts. The last member of a hard link set carries the
	// content.
	var next uint32
	last := make(map[*entry]string)
	for _, p := range names {
		e := b.files[p]
		o := e.inodeOwner
		if o.nlink == 0 {
			next++
			o.inode = next
		}
		e.inode = o.inode
		o.nlink++
		last[o] = p
	}

	cw := &countWriter{w: w}
	bw := bufio.NewWriterSize(cw, 1<<20)
	var files []File
	for _, p := range names {
		e := b.files[p]
		nlink := e.inodeOwner.nlink
		if e.typ == modeDir {
			nlink = 2
		}
		data := e.typ == modeReg && last[e.inodeOwner] == p
		size := e.size
		if e.typ == modeReg && !data {
			size = 0
		}
		if err := writeRecord(bw, p, e, nlink, size); err != nil {
			return nil, err
		}
		offset := cw.n + int64(bw.Buffered())
		digest, err := b.writeData(bw, p, e, data)
		if err != nil {
			return nil, err
		}
		if digest != nil {
			files = append(files, File{Offset: offset, Size: e.size, Digest: digest})
		}
	}
	if err := writeRecord(bw, trailerName, &entry{}, 1, 0); err != nil {
		return nil, err
	}
	if err := bw.Flush(); err != nil {
		return nil, err
	}
	return &Info{Size: cw.n, Entries: len(names), Files: files}, nil
}

// writeData writes the content of e, and returns its digest if the content is
// that of a regular file and the builder records digests.
func (b *Builder) writeData(w *bufio.Writer, name string, e *entry, data bool) ([]byte, error) {
	var n int64
	var digest []byte
	switch {
	case e.typ == modeSymlink:
		m, err := w.WriteString(e.target)
		if err != nil {
			return nil, err
		}
		n = int64(m)
	case data && e.size > 0:
		rc, err := e.src.open()
		if err != nil {
			return nil, fmt.Errorf("cpio: %s: %w", name, err)
		}
		defer rc.Close()
		var dst io.Writer = w
		var h hash.Hash
		if b.opts.DigestFiles {
			h = sha256.New()
			dst = io.MultiWriter(w, h)
		}
		copied, err := io.Copy(dst, io.LimitReader(rc, e.size))
		if err != nil {
			return nil, fmt.Errorf("cpio: %s: %w", name, err)
		}
		if copied != e.size {
			return nil, fmt.Errorf("cpio: %s: content is %d bytes, expected %d", name, copied, e.size)
		}
		n = copied
		if h != nil {
			digest = h.Sum(nil)
		}
	default:
		return nil, nil
	}
	_, err := w.Write(make([]byte, pad4(n)))
	return digest, err
}

func writeRecord(w *bufio.Writer, name string, e *entry, nlink uint32, size int64) error {
	var hdr [headerSize]byte
	copy(hdr[:], magic)
	for i := len(magic); i < headerSize; i++ {
		hdr[i] = '0'
	}
	put := func(i int, v uint32) {
		const hex = "0123456789abcdef"
		for j := 0; j < 8; j++ {
			hdr[6+i*8+j] = hex[(v>>(28-4*j))&0xf]
		}
	}
	// Fields: ino, mode, uid, gid, nlink, mtime, filesize, devmajor,
	// devminor, rdevmajor, rdevminor, namesize, check.
	put(0, e.inode)
	put(1, e.typ|e.perm)
	put(2, uint32(e.uid))
	put(3, uint32(e.gid))
	put(4, nlink)
	put(5, uint32(e.mtime))
	put(6, uint32(size))
	put(9, uint32(e.rmaj))
	put(10, uint32(e.rmin))
	put(11, uint32(len(name)+1))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.WriteString(name); err != nil {
		return err
	}
	// The terminating NUL, then padding to a multiple of four bytes for the
	// header and the name together.
	n := int64(headerSize + len(name) + 1)
	_, err := w.Write(make([]byte, 1+pad4(n)))
	return err
}
