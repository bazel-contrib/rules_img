package erofs

import (
	"fmt"
	"io"
	"io/fs"
	"maps"
	"math/bits"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/builder"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/disk"
)

// --- Exported types ---

// Writer is a writable filesystem that produces an EROFS image on Close.
// Files are added via Create, Mkdir, Symlink, and Mknod, then finalized
// by calling Close which serializes the complete EROFS image.
//
// A Writer built with NewWriter has no output attached and is finalized with
// Prepare and WriteTo instead, which emit a metadata-first image and need no
// seeking.
type Writer struct {
	out          io.WriteSeeker
	closed       bool
	blockSize    int    // 0 = unset, resolved to defaultBlockSize in Close
	buildTime    uint64 // from WithBuildTime or buildTimer
	buildTimeNs  uint32
	hasBuildTime bool
	uuid         [16]uint8           // from WithUUID; all-zero by default
	wErr         error               // sticky error: once set, all subsequent ops return it
	root         *fsEntry            // root directory
	byPath       map[string]*fsEntry // path → entry (all types)

	devices []uint64 // per-device block counts (one per MetadataOnly source)

	// Per-CopyFrom state, reset at the start of each CopyFrom call.
	copyMetadataOnly bool   // metadata-only for current CopyFrom
	copyMerge        bool   // merge mode: apply whiteouts
	copyDeviceID     uint16 // device ID assigned to current MetadataOnly CopyFrom

	inlineThreshold  int                                                             // from WithInlineThreshold
	syntheticDirMeta func(string, SyntheticDirMetadata) (SyntheticDirMetadata, bool) // from WithSyntheticDirMetadata

	// Set by Prepare; WriteTo emits exactly this layout.
	layout *Layout
	ew     *erofsWriter

	dataFile *os.File // external data file (nil = spool mode)
	dataOff  int64    // current byte offset in data file
	spool    *os.File // temp spool (created lazily)
	spoolOff int64    // current byte offset in spool
	tempDir  string   // from WithTempDir
	cpBuf    []byte   // shared buffer for io.Copy into File
	padBuf   []byte   // shared zero buffer for padding (block-sized, lazy)
}

// File is a writable regular file returned by Writer.Create.
// Data is written via Write or ReadFrom, then committed with Close.
type File struct {
	fs           *Writer
	entry        *fsEntry
	dataStartOff int64 // byte offset where this file's data begins
	written      int64
	closed       bool
}

// CreateOpt configures EROFS image creation.
type CreateOpt func(*createOptions)

// CopyOpt configures a CopyFrom operation.
type CopyOpt func(*Writer)

// --- Constructor ---

// Create returns a Writer that produces an EROFS image on Close.
// Options configure build time, data file, and temp directory.
func Create(out io.WriteSeeker, opts ...CreateOpt) *Writer {
	fsys := newWriter(opts...)
	fsys.out = out

	if fsys.dataFile != nil {
		// Reserve device slot 0 (DeviceID=1) for the data file.
		// MetadataOnly CopyFrom device IDs will start at slot 1+.
		// The reserved slot is filled in with the actual block count at Close.
		fsys.devices = append(fsys.devices, 0)
		off, err := fsys.dataFile.Seek(0, io.SeekEnd)
		if err == nil {
			fsys.dataOff = off
		}
	}

	return fsys
}

// NewWriter returns a Writer with no attached output. It is finalized with
// Prepare and WriteTo rather than Close, which produces a metadata-first image
// and therefore requires no seekable destination.
func NewWriter(opts ...CreateOpt) *Writer {
	return newWriter(opts...)
}

func newWriter(opts ...CreateOpt) *Writer {
	var o createOptions
	for _, opt := range opts {
		opt(&o)
	}

	root := &fsEntry{
		path: "/",
		mode: disk.StatTypeDir | 0o755,
	}
	fsys := &Writer{
		buildTime:        o.buildTime,
		buildTimeNs:      o.buildTimeNs,
		hasBuildTime:     o.hasBuildTime,
		uuid:             o.uuid,
		root:             root,
		byPath:           map[string]*fsEntry{"/": root},
		inlineThreshold:  o.inlineThreshold,
		syntheticDirMeta: o.syntheticDirMeta,
		dataFile:         o.dataFile,
		tempDir:          o.tempDir,
	}

	if o.blockSize != 0 {
		if err := fsys.setBlockSize(o.blockSize); err != nil {
			fsys.wErr = err
		}
	}

	return fsys
}

// --- CopyOpt functions ---

// MetadataOnly configures the current CopyFrom to emit only metadata.
// Regular files with pre-existing chunk mappings use chunk-based layout
// referencing an external device; file data is not copied.
func MetadataOnly() CopyOpt {
	return func(w *Writer) {
		w.copyMetadataOnly = true
	}
}

// Merge enables overlay merge semantics for the current CopyFrom.
// AUFS-style whiteout files (.wh.<name>) delete the named entry from
// prior layers, and opaque markers (.wh..wh..opq) delete all children
// of their parent directory. The whiteout entries themselves are not
// added to the image.
//
// When using Merge with a source containing AUFS whiteout files, do not
// pre-convert them; the Writer processes raw whiteout entries directly.
func Merge() CopyOpt {
	return func(w *Writer) {
		w.copyMerge = true
	}
}

// --- CreateOpt functions ---

// WithBlockSize sets the filesystem block size. The value must be a power
// of two between 512 and 64 KiB. When unset the default is 4096.
// An invalid size causes subsequent Writer operations to return an error.
// If CopyFrom is called with a source that declares a different block size,
// CopyFrom returns an error.
func WithBlockSize(n int) CreateOpt {
	return func(o *createOptions) {
		o.blockSize = n
	}
}

// WithBuildTime sets the filesystem build timestamp.
func WithBuildTime(sec uint64, nsec uint32) CreateOpt {
	return func(o *createOptions) {
		o.buildTime = sec
		o.buildTimeNs = nsec
		o.hasBuildTime = true
	}
}

// WithDataFile sets an external data file for metadata-only mode.
// File.Write appends to this file at block-aligned offsets; chunk
// indexes reference those blocks with DeviceID=1.
func WithDataFile(f *os.File) CreateOpt {
	return func(o *createOptions) {
		o.dataFile = f
	}
}

// WithTempDir overrides the temp directory for the spool file.
// Only used when no data file is provided.
func WithTempDir(dir string) CreateOpt {
	return func(o *createOptions) {
		o.tempDir = dir
	}
}

// WithUUID sets the filesystem UUID. The default is all zero, which is already
// deterministic; set an explicit value to distinguish images that are otherwise
// byte-identical.
func WithUUID(uuid [16]byte) CreateOpt {
	return func(o *createOptions) {
		o.uuid = uuid
	}
}

// WithInlineThreshold caps tail-packing of regular file payloads into their
// inodes, so the caller decides which files get a data extent of their own
// (and therefore appear in [Layout.Extents]).
//
// A file is tail-packed only if its size is below n and it still fits in the
// remainder of the inode's block. n == 0 (the default) keeps the implicit
// "whatever fits" rule; a negative n disables tail packing entirely. Directory
// blocks and symlink targets are unaffected.
func WithInlineThreshold(n int) CreateOpt {
	return func(o *createOptions) {
		o.inlineThreshold = n
	}
}

// SyntheticDirMetadata are the attributes given to a directory the Writer has
// to synthesize because an entry beneath it was added without an entry of its
// own.
type SyntheticDirMetadata struct {
	// Mode is the permission bits; the directory type bit is added by the Writer.
	Mode uint16
	// UID and GID are the numeric owner ids.
	UID, GID uint32
	// Mtime and MtimeNs are the modification timestamp.
	Mtime   uint64
	MtimeNs uint32
	// Xattrs are extended attributes, or nil.
	Xattrs map[string]string
}

// WithSyntheticDirMetadata installs a hook consulted whenever the Writer has to
// synthesize a missing parent directory, so the caller can give it meaningful
// attributes instead of the default 0755 root:root.
//
// fn receives the image path of the directory to synthesize and the attributes
// of the nearest ancestor directory already present in the image (the root's
// attributes if there is no closer one), which is what mkfs.erofs inherits
// from. Returning false falls back to the default attributes.
//
// A directory that is later described explicitly (by Mkdir or CopyFrom) has its
// synthesized attributes replaced, so the hook only decides the attributes of
// directories that are never described at all. [Writer.SyntheticDirs] reports
// which those are.
func WithSyntheticDirMetadata(fn func(path string, ancestor SyntheticDirMetadata) (SyntheticDirMetadata, bool)) CreateOpt {
	return func(o *createOptions) {
		o.syntheticDirMeta = fn
	}
}

// --- Writer entry methods ---

// Create creates a regular file with default mode 0644. The caller must
// Close the returned File.
func (fsys *Writer) Create(name string) (*File, error) {
	if fsys.wErr != nil {
		return nil, fsys.wErr
	}
	name = cleanPath(name)
	if name == "/" {
		return nil, fmt.Errorf("mkfs: cannot create file at root")
	}
	if err := fsys.checkPath(name); err != nil {
		return nil, err
	}

	fsys.ensureParent(name)

	e := &fsEntry{
		path: name,
		mode: disk.StatTypeReg | 0o644,
	}
	fsys.addChild(e)

	f := &File{
		fs:    fsys,
		entry: e,
	}

	if fsys.dataFile != nil {
		f.dataStartOff = fsys.dataOff
		e.dataStartOff = fsys.dataOff
	} else {
		if err := fsys.ensureSpool(); err != nil {
			return nil, err
		}
		f.dataStartOff = fsys.spoolOff
		e.spoolOff = fsys.spoolOff
		e.dataStartOff = fsys.spoolOff
	}

	return f, nil
}

// AddFile adds a regular file (mode 0644) whose content is read from r when the
// image is serialized. Nothing is copied or buffered now: r is read exactly
// once, in order, by Close or WriteTo, so a caller with the content already on
// disk can hand over a reader that opens the file lazily and pay for no spool
// copy at all. If r implements io.Closer it is closed after being read.
//
// size must equal the number of bytes r will yield; a mismatch is reported when
// the image is written.
func (fsys *Writer) AddFile(name string, size int64, r io.Reader) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	if size < 0 {
		return fmt.Errorf("mkfs: negative size %d for %q", size, name)
	}
	name = cleanPath(name)
	if name == "/" {
		return fmt.Errorf("mkfs: cannot create file at root")
	}
	if err := fsys.checkPath(name); err != nil {
		return err
	}

	fsys.ensureParent(name)

	e := &fsEntry{
		path:       name,
		mode:       disk.StatTypeReg | 0o644,
		size:       uint64(size),
		fileClosed: true,
	}
	if size > 0 {
		e.directData = r
	} else if c, ok := r.(io.Closer); ok {
		_ = c.Close()
	}
	fsys.addChild(e)
	return nil
}

// MkdirSynthesized creates a directory the caller does not describe, giving it
// the attributes [WithSyntheticDirMetadata] chooses (or the default 0755
// root-owned) and reporting it from [Writer.SyntheticDirs] — exactly as if it
// had been invented to hold an entry beneath it.
//
// It is for a caller that needs a directory to exist without claiming to know
// what it should look like: to hang an extended attribute on, say. An entry that
// already exists at name is left untouched.
func (fsys *Writer) MkdirSynthesized(name string) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	name = cleanPath(name)
	if name == "/" {
		return nil
	}
	if _, ok := fsys.byPath[name]; ok {
		return nil
	}
	if err := fsys.checkMutable(); err != nil {
		return err
	}
	fsys.ensureParent(name)
	fsys.addChild(fsys.synthesizedDir(name, fsys.byPath[path.Dir(name)]))
	return nil
}

// Mkdir creates a directory. Only permission bits from perm are used,
// including setuid, setgid and sticky as os.Mkdir does; type bits are forced
// to directory. Mkdir("/", perm) sets root permissions.
//
// A directory that already exists — including one the Writer synthesized to
// hold an entry added earlier — has its permissions updated in place and is no
// longer reported by [Writer.SyntheticDirs]. Any other existing entry at name
// is an error.
func (fsys *Writer) Mkdir(name string, perm fs.FileMode) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	dirMode := disk.StatTypeDir | goModeToUnixMode(perm)&0o7777
	name = cleanPath(name)
	if name == "/" {
		fsys.root.mode = dirMode
		return nil
	}
	if existing, ok := fsys.byPath[name]; ok {
		if err := fsys.checkMutable(); err != nil {
			return err
		}
		if existing.mode&disk.StatTypeMask != disk.StatTypeDir {
			return fmt.Errorf("mkfs: %q already exists and is not a directory", name)
		}
		existing.mode = disk.StatTypeDir | uint16(perm.Perm())
		existing.synthesized = false
		return nil
	}
	if err := fsys.checkPath(name); err != nil {
		return err
	}

	fsys.ensureParent(name)

	e := &fsEntry{
		path: name,
		mode: dirMode,
	}
	fsys.addChild(e)

	return nil
}

// Symlink creates newname as a symbolic link to oldname (mode 0777).
func (fsys *Writer) Symlink(oldname, newname string) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	newname = cleanPath(newname)
	if newname == "/" {
		return fmt.Errorf("mkfs: cannot create symlink at root")
	}
	if err := fsys.checkPath(newname); err != nil {
		return err
	}

	fsys.ensureParent(newname)

	e := &fsEntry{
		path:       newname,
		mode:       disk.StatTypeSymlink | 0o777,
		linkTarget: oldname,
	}
	fsys.addChild(e)

	return nil
}

// Mknod creates a device, FIFO, or socket. mode must include type bits
// (e.g. [ModeChardev] | 0o666).
func (fsys *Writer) Mknod(name string, mode uint16, rdev uint32) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	name = cleanPath(name)
	if name == "/" {
		return fmt.Errorf("mkfs: cannot mknod at root")
	}
	if err := fsys.checkPath(name); err != nil {
		return err
	}

	fsys.ensureParent(name)

	e := &fsEntry{
		path: name,
		mode: mode,
		rdev: rdev,
	}
	fsys.addChild(e)

	return nil
}

// --- Writer metadata methods ---

// Chmod sets permission bits on the named path, preserving type bits.
func (fsys *Writer) Chmod(name string, mode fs.FileMode) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	perm := goModeToUnixMode(mode) & 0o7777
	e.mode = (e.mode & disk.StatTypeMask) | perm
	return nil
}

// Chown sets the owner UID and GID on the named path.
func (fsys *Writer) Chown(name string, uid, gid int) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	e.uid = uint32(uid)
	e.gid = uint32(gid)
	return nil
}

// Chtimes sets the access and modification times on the named path.
// EROFS only stores mtime; atime is retained for read-back before Close.
func (fsys *Writer) Chtimes(name string, atime time.Time, mtime time.Time) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	e.atime = uint64(atime.Unix())
	e.atimeNs = uint32(atime.Nanosecond())
	e.mtime = uint64(mtime.Unix())
	e.mtimeNs = uint32(mtime.Nanosecond())
	return nil
}

// Setxattr sets an extended attribute on the named path.
func (fsys *Writer) Setxattr(name, attr, value string) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	if e.xattrs == nil {
		e.xattrs = make(map[string]string)
	}
	e.xattrs[attr] = value
	return nil
}

// SetNlink overrides the computed link count on the named path.
func (fsys *Writer) SetNlink(name string, nlink uint32) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	e.nlink = nlink
	e.nlinkSet = true
	return nil
}

// SetToken attaches an opaque value to the named entry. If the entry ends up
// with an out-of-line payload, the value is reported as [Extent.Token] on the
// corresponding entry of the layout returned by [Writer.Prepare], which lets a
// caller recover per-entry context (for example a precomputed content digest)
// without matching on paths.
func (fsys *Writer) SetToken(name string, tok any) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	e, err := fsys.lookup(name)
	if err != nil {
		return err
	}
	e.token = tok
	return nil
}

// Link creates newname as a hardlink to oldname: a second directory entry
// resolving to the same inode, with the inode's link count incremented.
// Metadata is necessarily shared. Directories cannot be hardlinked.
func (fsys *Writer) Link(oldname, newname string) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	if err := fsys.checkMutable(); err != nil {
		return err
	}
	target, err := fsys.lookup(oldname)
	if err != nil {
		return err
	}
	target = resolveHardlink(target)
	if target.mode&disk.StatTypeMask == disk.StatTypeDir {
		return fmt.Errorf("mkfs: cannot hardlink directory %q", oldname)
	}
	newname = cleanPath(newname)
	if newname == "/" {
		return fmt.Errorf("mkfs: cannot create hardlink at root")
	}
	if err := fsys.checkPath(newname); err != nil {
		return err
	}

	fsys.ensureParent(newname)

	e := &fsEntry{
		path:       newname,
		mode:       target.mode,
		size:       target.size,
		hardlinkTo: target,
		fileClosed: true,
	}
	fsys.addChild(e)
	target.extraLinks++
	return nil
}

// ShareData makes dst's inode reference the same data extent as src, so the
// payload is stored once. Both inodes keep independent metadata; dst's size is
// set to src's. src must be a regular file with a payload; a shared payload is
// never tail-packed into an inode, and appears exactly once in
// [Layout.Extents].
func (fsys *Writer) ShareData(src, dst string) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	if err := fsys.checkMutable(); err != nil {
		return err
	}
	s, err := fsys.lookup(src)
	if err != nil {
		return err
	}
	d, err := fsys.lookup(dst)
	if err != nil {
		return err
	}
	s, d = resolveHardlink(s), resolveHardlink(d)
	for s.shareSrc != nil {
		s = s.shareSrc
	}
	if s == d {
		return fmt.Errorf("mkfs: cannot share %q with itself", dst)
	}
	if s.mode&disk.StatTypeMask != disk.StatTypeReg || d.mode&disk.StatTypeMask != disk.StatTypeReg {
		return fmt.Errorf("mkfs: can only share data between regular files (%q, %q)", src, dst)
	}
	if s.size == 0 {
		return fmt.Errorf("mkfs: cannot share data of empty file %q", src)
	}
	if len(s.chunks) > 0 || s.metadataOnly {
		return fmt.Errorf("mkfs: cannot share data of chunk-based file %q", src)
	}
	s.shareRefs++
	d.shareSrc = s
	d.size = s.size
	d.directData = nil
	d.chunks = nil
	d.fileClosed = true
	return nil
}

// Remove deletes the entry at name, and for a directory its whole subtree, from
// the pending image. It is the primitive behind last-writer-wins semantics: a
// caller that has to replace an entry removes it first.
//
// Removing an entry that a hardlink or a shared payload still points at is an
// error, because that would leave a dangling reference.
func (fsys *Writer) Remove(name string) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	if err := fsys.checkMutable(); err != nil {
		return err
	}
	name = cleanPath(name)
	if name == "/" {
		return fmt.Errorf("mkfs: cannot remove root")
	}
	e, ok := fsys.byPath[name]
	if !ok {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	if err := checkUnreferenced(e); err != nil {
		return err
	}
	if e.mode&disk.StatTypeMask == disk.StatTypeDir {
		for _, c := range e.children {
			if c.removed {
				continue
			}
			if err := checkUnreferenced(c); err != nil {
				return err
			}
		}
	}
	if e.hardlinkTo != nil {
		e.hardlinkTo.extraLinks--
	}
	if e.shareSrc != nil {
		e.shareSrc.shareRefs--
	}
	fsys.remove(name)
	return nil
}

// SyntheticDirs returns the sorted image paths of directories the Writer
// synthesized because an entry beneath them was added without an entry of their
// own, and which were never described explicitly afterwards. A caller that
// cares about lower-layer directory attributes (an EROFS image is a filesystem,
// not a changeset, so the dirent tree has to be complete) can use this to warn
// or fail.
func (fsys *Writer) SyntheticDirs() []string {
	var paths []string
	for p, e := range fsys.byPath {
		if e.synthesized && !e.removed {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths
}

// checkMutable reports an error if the tree can no longer be changed.
func (fsys *Writer) checkMutable() error {
	if fsys.closed {
		return fmt.Errorf("mkfs: FS is closed")
	}
	if fsys.layout != nil {
		return fmt.Errorf("mkfs: layout already prepared")
	}
	return nil
}

// resolveHardlink follows a chain of hardlinks to the inode they name.
func resolveHardlink(e *fsEntry) *fsEntry {
	for e.hardlinkTo != nil {
		e = e.hardlinkTo
	}
	return e
}

// checkUnreferenced reports an error if removing e would dangle a hardlink or a
// shared payload.
func checkUnreferenced(e *fsEntry) error {
	if e.extraLinks > 0 {
		return fmt.Errorf("mkfs: %q still has %d hardlink(s) to it", e.path, e.extraLinks)
	}
	if e.shareRefs > 0 {
		return fmt.Errorf("mkfs: %q payload is still shared by %d entr(ies)", e.path, e.shareRefs)
	}
	return nil
}

// --- Writer bulk copy ---

// CopyFrom walks an fs.FS and adds all entries.
// Opens files for data when Entry.Data is nil.
// Reads symlink targets via readLinker interface when Entry.LinkTarget is empty.
// If src implements blockSizer, the image block size is set accordingly.
func (fsys *Writer) CopyFrom(src fs.FS, opts ...CopyOpt) error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	// Reset per-CopyFrom state.
	fsys.copyMetadataOnly = false
	fsys.copyMerge = false
	fsys.copyDeviceID = 0
	for _, opt := range opts {
		opt(fsys)
	}
	// Detect EROFS image source for direct metadata/chunk extraction.
	// The fast path (copyFromImage) only applies to MetadataOnly mode
	// where no file data needs to be read — just inodes, dirents, and
	// chunk indexes. For non-MetadataOnly, fall through to the fs.WalkDir
	// path which opens files for data.
	if srcImg, ok := src.(*image); ok {
		if err := fsys.setBlockSize(int(srcImg.blockSize())); err != nil {
			return err
		}
		if !fsys.hasBuildTime {
			fsys.buildTime = srcImg.buildTime()
			fsys.hasBuildTime = true
		}
		if fsys.copyMetadataOnly {
			devBlocks := srcImg.deviceBlocks()
			fsys.devices = append(fsys.devices, devBlocks...)
			fsys.copyDeviceID = uint16(len(fsys.devices) - len(devBlocks) + 1)
			return fsys.copyFromImage(srcImg)
		}
	}
	if bs, ok := src.(blockSizer); ok {
		if err := fsys.setBlockSize(int(bs.BlockSize())); err != nil {
			return err
		}
	}
	if fsys.copyMetadataOnly {
		if db, ok := src.(deviceBlocker); ok {
			fsys.devices = append(fsys.devices, db.DeviceBlocks())
			fsys.copyDeviceID = uint16(len(fsys.devices))
		}
	}
	if bt, ok := src.(buildTimer); ok && !fsys.hasBuildTime {
		fsys.buildTime = bt.BuildTime()
		fsys.hasBuildTime = true
	}
	return fs.WalkDir(src, ".", func(fpath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", fpath, err)
		}

		// Normalize path to absolute
		p := "/" + fpath
		if fpath == "." {
			p = "/"
		}

		// Merge mode: process whiteout markers.
		if fsys.copyMerge && p != "/" {
			base := path.Base(p)
			if strings.HasPrefix(base, whiteoutPrefix) {
				if base == opaqueWhiteout {
					// Opaque directory: remove all prior children of parent.
					fsys.removeChildren(path.Dir(p))
				} else {
					// File whiteout: remove the named entry.
					target := path.Join(path.Dir(p), base[len(whiteoutPrefix):])
					fsys.remove(target)
				}
				return nil
			}
		}

		// Extract extended metadata from Sys().
		var be *builder.Entry
		switch sys := info.Sys().(type) {
		case *builder.Entry:
			be = sys
		case *Stat:
			// EROFS image source: convert *Stat to *builder.Entry.
			be = &builder.Entry{
				UID:     sys.UID,
				GID:     sys.GID,
				Mtime:   sys.Mtime,
				MtimeNs: sys.MtimeNs,
				Nlink:   uint32(sys.Nlink),
				Rdev:    sys.Rdev,
				Xattrs:  sys.Xattrs,
			}
		}

		// For regular files, get a data reader.
		if info.Mode().IsRegular() && info.Size() > 0 && (be == nil || be.Data == nil) {
			// In metadata-only mode, data is referenced via chunk indexes
			// from the source — no need to open the file.
			if fsys.copyMetadataOnly {
				if be == nil {
					be = entryFromSys(info)
					if be == nil {
						be = &builder.Entry{}
					}
				}
				// Generate chunks from DataRange if available.
				if len(be.Chunks) == 0 {
					if dr, ok := info.(dataRanger); ok {
						if ranges := dr.DataRange(); len(ranges) > 0 {
							chunks, err := fsys.chunksFromRanges(ranges, info.Size())
							if err != nil {
								return fmt.Errorf("chunksFromRanges %s: %w", p, err)
							}
							be.Chunks = chunks
							// Contiguous: a single non-hole range whose total-size
							// invariant is satisfied (guaranteed by chunksFromRanges)
							// means the file is fully covered by one contiguous extent.
							be.Contiguous = len(ranges) == 1 && ranges[0].Offset != holeOffset
						}
					}
				}
				return fsys.add(p, &entryFileInfo{info: info, sys: be})
			}
			// For EROFS sources, use direct SectionReader (bypasses
			// block-at-a-time reader for contiguous flat-plain data).
			if srcImg, ok := src.(*image); ok {
				if st, ok := info.Sys().(*Stat); ok {
					f := file{img: srcImg, nid: uint64(st.Ino)}
					if ino, err := f.readInfo(); err == nil {
						if dr := srcImg.openDirect(ino); dr != nil {
							if be == nil {
								be = &builder.Entry{}
							}
							be.Data = dr
							return fsys.add(p, &entryFileInfo{info: info, sys: be})
						}
					}
				}
			}
			f, err := src.Open(fpath)
			if err != nil {
				return fmt.Errorf("open %s: %w", fpath, err)
			}
			if be == nil {
				be = entryFromSys(info)
				if be == nil {
					be = &builder.Entry{}
				}
			}
			be.Data = f.(io.Reader)
			return fsys.add(p, &entryFileInfo{info: info, sys: be})
		}

		// For symlinks without LinkTarget, read via ReadLink interface.
		if info.Mode()&fs.ModeSymlink != 0 && (be == nil || be.LinkTarget == "") {
			if rl, ok := src.(readLinker); ok {
				target, err := rl.ReadLink(fpath)
				if err != nil {
					return fmt.Errorf("readlink %s: %w", fpath, err)
				}
				if be == nil {
					be = entryFromSys(info)
					if be == nil {
						be = &builder.Entry{}
					}
				}
				be.LinkTarget = target
				return fsys.add(p, &entryFileInfo{info: info, sys: be})
			}
		}

		// For directories, ensure nlink >= 2.
		if info.Mode().IsDir() {
			if be == nil {
				be = entryFromSys(info)
				if be == nil {
					be = &builder.Entry{Nlink: 2}
				}
			}
			if be.Nlink < 2 {
				be.Nlink = 2
			}
			return fsys.add(p, &entryFileInfo{info: info, sys: be})
		}

		// General case: devices, fifos, sockets, etc.
		// Wrap in entryFileInfo when be was extracted from Sys()
		// so that add() sees the metadata.
		if be != nil {
			return fsys.add(p, &entryFileInfo{info: info, sys: be})
		}
		return fsys.add(p, info)
	})
}

// --- Writer finalization ---

// Close writes the EROFS image. The FS must not be used after Close.
func (fsys *Writer) Close() error {
	if fsys.wErr != nil {
		return fsys.wErr
	}
	if fsys.closed {
		return fmt.Errorf("mkfs: FS already closed")
	}
	if fsys.out == nil {
		return fmt.Errorf("mkfs: no output attached; finalize with WriteTo")
	}
	if fsys.layout != nil {
		return fmt.Errorf("mkfs: layout already prepared; finalize with WriteTo")
	}
	fsys.closed = true

	if fsys.spool != nil {
		defer func() { _ = fsys.spool.Close() }()
	}

	ew, err := fsys.finalizeTree()
	if err != nil {
		return err
	}

	// Data-first layout: sbArea, data blocks, metadata. The sentinel makes
	// assignDataBlocks place data before metadata.
	ew.metaBlkAddr = 0xFFFFFFFF
	ew.assignDataBlocks()
	if err := ew.resolveSharedData(); err != nil {
		return err
	}

	return ew.write(fsys.out)
}

// finalizeTree resolves the block size, converts the fsEntry tree into the
// erofsEntry tree and plans the metadata area. The caller decides where data
// blocks go by setting erofsWriter.metaBlkAddr and calling assignDataBlocks.
func (fsys *Writer) finalizeTree() (*erofsWriter, error) {
	fsys.resolveBlockSize()

	if fsys.dataFile != nil {
		// Fill in the reserved device slot 0 with the actual block count.
		blocks := (fsys.dataOff + int64(fsys.blockSize) - 1) / int64(fsys.blockSize)
		fsys.devices[0] = uint64(blocks)
	}

	buildTime := fsys.buildTime
	if !fsys.hasBuildTime {
		buildTime = uint64(time.Now().Unix())
	}

	// Build erofsEntry tree from the fsEntry tree via BFS.
	root, err := fsys.buildErofsTree()
	if err != nil {
		return nil, err
	}

	var chunkBits uint8
	for cs := fsys.blockSize; cs < 4096; cs <<= 1 {
		chunkBits++
	}

	ew := &erofsWriter{
		buildTime:       buildTime,
		buildTimeNs:     fsys.buildTimeNs,
		uuid:            fsys.uuid,
		devices:         fsys.devices,
		blockSize:       fsys.blockSize,
		chunkBits:       chunkBits,
		inlineThreshold: fsys.inlineThreshold,
		zeroBuf:         make([]byte, fsys.blockSize),
	}

	ew.planLayout(root)
	fixParentNids(root, root)
	return ew, nil
}

// Stat returns file info for the named path. The name is cleaned the same
// way as other Writer methods (leading slash, no trailing slash).
func (fsys *Writer) Stat(name string) (fs.FileInfo, error) {
	name = cleanPath(name)
	e, ok := fsys.byPath[name]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return &writerFileInfo{entry: e}, nil
}

// Open opens the named file for reading. For regular files, the file must
// have been closed (data finalized) before it can be opened for reading.
// For directories, the returned file implements fs.ReadDirFile.
func (fsys *Writer) Open(name string) (fs.File, error) {
	name = cleanPath(name)
	e, ok := fsys.byPath[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	// A hardlink reads through to the inode it names.
	e = resolveHardlink(e)
	if e.shareSrc != nil {
		e = e.shareSrc
	}

	typ := e.mode & disk.StatTypeMask
	switch typ {
	case disk.StatTypeDir:
		return &readDir{fsys: fsys, entry: e}, nil

	case disk.StatTypeReg:
		if !e.fileClosed {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fmt.Errorf("file not yet closed for writing")}
		}
		var sr *io.SectionReader
		if fsys.dataFile != nil {
			sr = io.NewSectionReader(fsys.dataFile, e.dataStartOff, int64(e.size))
		} else if fsys.spool != nil && e.size > 0 {
			sr = io.NewSectionReader(fsys.spool, e.dataStartOff, int64(e.size))
		}
		return &readFile{entry: e, reader: sr}, nil

	default:
		// Symlinks, devices, etc.: stat-only, no readable data.
		return &readFile{entry: e}, nil
	}
}

// --- File methods ---

// Write appends data to the file.
func (f *File) Write(p []byte) (int, error) {
	if f.closed {
		return 0, fmt.Errorf("mkfs: write to closed file")
	}

	if f.fs.dataFile != nil {
		n, err := f.fs.dataFile.Write(p)
		f.written += int64(n)
		f.fs.dataOff += int64(n)
		return n, err
	}

	n, err := f.fs.spool.Write(p)
	f.written += int64(n)
	f.fs.spoolOff += int64(n)
	return n, err
}

// ReadFrom implements io.ReaderFrom, allowing io.Copy(f, src) to use
// a shared buffer instead of allocating a new 32KB buffer per call.
func (f *File) ReadFrom(r io.Reader) (int64, error) {
	buf := f.fs.copyBuf()
	var written int64
	for {
		nr, er := r.Read(buf)
		if nr > 0 {
			nw, ew := f.Write(buf[:nr])
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}

// Close commits the file entry. For data file mode, pads to block
// boundary and records chunk indexes.
func (f *File) Close() error {
	if f.closed {
		return fmt.Errorf("mkfs: file already closed")
	}
	f.closed = true
	f.entry.fileClosed = true
	f.entry.size = uint64(f.written)

	if f.fs.dataFile != nil {
		return f.closeDataFile()
	}
	return nil
}

// Chmod sets permission bits on the file, matching os.File.Chmod.
func (f *File) Chmod(mode fs.FileMode) error {
	perm := goModeToUnixMode(mode) & 0o7777
	f.entry.mode = (f.entry.mode & disk.StatTypeMask) | perm
	return nil
}

// Chown sets the owner UID and GID on the file, matching os.File.Chown.
func (f *File) Chown(uid, gid int) error {
	f.entry.uid = uint32(uid)
	f.entry.gid = uint32(gid)
	return nil
}

// --- Internal types ---

// fsEntry is the in-memory representation of a filesystem entry held by Writer.
type fsEntry struct {
	path       string
	mode       uint16
	uid, gid   uint32
	atime      uint64
	atimeNs    uint32
	mtime      uint64
	mtimeNs    uint32
	nlink      uint32
	nlinkSet   bool // true if SetNlink was called
	size       uint64
	rdev       uint32
	xattrs     map[string]string
	linkTarget string
	chunks     []builder.Chunk
	contiguous bool // data blocks are contiguous; flat-plain is sufficient
	token      any  // from SetToken; surfaced as Extent.Token

	// Tree structure — maintained during add/remove.
	parent   *fsEntry
	children []*fsEntry

	// hardlinkTo, when set, marks this entry as an additional dirent for
	// another inode (see Link). It gets no inode of its own.
	hardlinkTo *fsEntry
	extraLinks uint32 // number of hardlinks pointing at this entry
	// shareSrc, when set, makes this inode reference another's data extent
	// (see ShareData).
	shareSrc  *fsEntry
	shareRefs uint32 // number of entries sharing this entry's payload

	// data location in spool file
	spoolOff     int64
	dataStartOff int64     // byte offset where file data begins (spool or data file)
	fileClosed   bool      // true after File.Close() is called
	directData   io.Reader // bypasses spool; set by add() for source-provided data

	removed      bool // true if removed by a whiteout in a merge layer
	metadataOnly bool // from a metadata-only CopyFrom; use chunk-based layout
	synthesized  bool // created by ensureParent, never described explicitly
}

// createOptions holds the parsed option values for Create.
type createOptions struct {
	buildTime        uint64
	buildTimeNs      uint32
	hasBuildTime     bool
	uuid             [16]uint8
	blockSize        int      // 0 = use default
	dataFile         *os.File // external data file for metadata-only mode
	tempDir          string   // temp directory for spool file
	inlineThreshold  int      // see WithInlineThreshold
	syntheticDirMeta func(string, SyntheticDirMetadata) (SyntheticDirMetadata, bool)
}

// blockSizer may be implemented by an fs.FS to declare its block size.
// Writer.CopyFrom uses this to set the image block size automatically.
type blockSizer interface {
	BlockSize() uint32
}

// buildTimer may be implemented by an fs.FS to suggest a build timestamp.
// If the caller hasn't set WithBuildTime, CopyFrom uses this value.
// Entries whose mtime matches the build time can use compact (32-byte) inodes.
type buildTimer interface {
	BuildTime() uint64
}

// deviceBlocker may be implemented by an fs.FS to declare the total
// block count of its backing device. Writer.CopyFrom uses this to
// configure the device slot for metadata-only mode.
type deviceBlocker interface {
	DeviceBlocks() uint64
}

// readLinker is an interface for filesystems that support reading symlink targets.
type readLinker interface {
	ReadLink(name string) (string, error)
}

// dataRanger may be implemented by fs.FileInfo to provide the physical
// location of uncompressed file data in backing devices. CopyFrom checks
// this via type assertion in metadata-only mode to build chunk indexes
// without requiring the caller to construct internal chunk types.
//
// This interface should only be implemented for files whose device data
// is stored verbatim (uncompressed). For compressed files, return nil or
// do not implement the interface. In full-image mode CopyFrom then falls
// back to reading through Open(), which decompresses transparently. In
// MetadataOnly mode there is no such fallback: the file is stored as a
// chunk-based inode with no physical mappings (all holes).
type dataRanger interface {
	DataRange() []DataRange
}

// --- Internal types ---

// erofsEntry is the internal representation of a file/dir/symlink used by the builder.
type erofsEntry struct {
	mode    uint16
	uid     uint32
	gid     uint32
	mtime   uint64
	mtimeNs uint32
	nlink   uint32
	size    uint64
	rdev    uint32

	name      string
	path      string
	children  []*erofsEntry
	symTarget string

	// For regular files — metadata-only mode
	chunks       []builder.Chunk
	contiguous   bool  // data blocks are contiguous; use large chunk size
	chunkBits    uint8 // per-entry chunk bits (0 = use global)
	metadataOnly bool  // chunk-based layout even without chunks

	// For regular files — full-image mode
	data io.Reader

	// Extended attributes
	xattrs map[string]string

	// hardlinkTo, when set, marks this entry as an extra dirent for another
	// inode: it gets no inode (and no NID) of its own.
	hardlinkTo *erofsEntry
	// shareSrc, when set, makes this inode reference another's data extent.
	shareSrc *erofsEntry
	// isShareSrc marks an entry whose payload other inodes reference, which
	// keeps it out of line.
	isShareSrc bool
	// token is the caller value from Writer.SetToken.
	token any

	// EROFS layout (assigned during planning)
	nid           uint64
	parentNid     uint64
	erofsFileType uint8
	layout        uint8
	compact       bool // true = 32-byte compact inode; false = 64-byte extended
	xattrSize     int  // bytes of xattr area (0 if no xattrs)
	chunkPad      int  // 8-byte alignment pad between xattr area and chunk-index map
	trailingSize  int

	// Data block address for flat-plain files (full-image mode)
	dataBlkAddr uint32
}

// direntTarget returns the inode an entry's dirent resolves to: itself, or the
// hardlink target when the entry is an extra dirent for another inode.
func direntTarget(e *erofsEntry) *erofsEntry {
	if e.hardlinkTo != nil {
		return e.hardlinkTo
	}
	return e
}

// --- Internal helpers ---

// cleanPath normalizes a filesystem path to an absolute rooted form.
func cleanPath(p string) string {
	if p == "" || p == "." || p == "/" {
		return "/"
	}
	p = path.Clean(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// fixParentNids sets the parent NID in the ".." dirent for all directories.
// This must be called after planLayout has assigned NIDs.
func fixParentNids(e *erofsEntry, parent *erofsEntry) {
	e.parentNid = parent.nid
	for _, c := range e.children {
		if c.mode&disk.StatTypeMask == disk.StatTypeDir {
			fixParentNids(c, e)
		}
	}
}

// entryFileInfo wraps an fs.FileInfo but overrides Sys() to return a *builder.Entry.
type entryFileInfo struct {
	info fs.FileInfo
	sys  *builder.Entry
}

func (fi *entryFileInfo) Name() string       { return fi.info.Name() }
func (fi *entryFileInfo) Size() int64        { return fi.info.Size() }
func (fi *entryFileInfo) Mode() fs.FileMode  { return fi.info.Mode() }
func (fi *entryFileInfo) ModTime() time.Time { return fi.info.ModTime() }
func (fi *entryFileInfo) IsDir() bool        { return fi.info.IsDir() }
func (fi *entryFileInfo) Sys() any           { return fi.sys }

// writerFileInfo implements fs.FileInfo for an fsEntry.
type writerFileInfo struct {
	entry *fsEntry
}

func (fi *writerFileInfo) Name() string      { return path.Base(fi.entry.path) }
func (fi *writerFileInfo) Size() int64       { return int64(fi.entry.size) }
func (fi *writerFileInfo) Mode() fs.FileMode { return disk.EroFSModeToGoFileMode(fi.entry.mode) }
func (fi *writerFileInfo) ModTime() time.Time {
	return time.Unix(int64(fi.entry.mtime), int64(fi.entry.mtimeNs))
}
func (fi *writerFileInfo) IsDir() bool { return fi.entry.mode&disk.StatTypeMask == disk.StatTypeDir }
func (fi *writerFileInfo) Sys() any    { return nil }

// readFile implements fs.File for reading back a finalized file's data.
type readFile struct {
	entry  *fsEntry
	reader *io.SectionReader // nil for empty files or non-regular types
	closed bool
}

func (f *readFile) Stat() (fs.FileInfo, error) {
	return &writerFileInfo{entry: f.entry}, nil
}

func (f *readFile) Read(p []byte) (int, error) {
	if f.closed {
		return 0, fmt.Errorf("mkfs: read from closed file")
	}
	if f.reader == nil {
		return 0, io.EOF
	}
	return f.reader.Read(p)
}

func (f *readFile) Close() error {
	if f.closed {
		return fmt.Errorf("mkfs: file already closed")
	}
	f.closed = true
	return nil
}

// readDir implements fs.ReadDirFile for a directory in Writer.
type readDir struct {
	fsys     *Writer
	entry    *fsEntry
	children []fs.DirEntry // lazily populated
	offset   int
	closed   bool
}

func (d *readDir) Stat() (fs.FileInfo, error) {
	return &writerFileInfo{entry: d.entry}, nil
}

func (d *readDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.entry.path, Err: fmt.Errorf("is a directory")}
}

func (d *readDir) Close() error {
	if d.closed {
		return fmt.Errorf("mkfs: dir already closed")
	}
	d.closed = true
	return nil
}

func (d *readDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.closed {
		return nil, fmt.Errorf("mkfs: read from closed dir")
	}
	if d.children == nil {
		d.children = d.collectChildren()
	}

	if n <= 0 {
		entries := d.children[d.offset:]
		d.offset = len(d.children)
		return entries, nil
	}

	remaining := d.children[d.offset:]
	if len(remaining) == 0 {
		return nil, io.EOF
	}
	if n > len(remaining) {
		n = len(remaining)
	}
	entries := remaining[:n]
	d.offset += n
	if d.offset >= len(d.children) {
		return entries, io.EOF
	}
	return entries, nil
}

func (d *readDir) collectChildren() []fs.DirEntry {
	children := make([]fs.DirEntry, 0, len(d.entry.children))
	for _, e := range d.entry.children {
		if e.removed {
			continue
		}
		children = append(children, &dirEntry{entry: e})
	}
	sort.Slice(children, func(i, j int) bool {
		return children[i].Name() < children[j].Name()
	})
	return children
}

// dirEntry implements fs.DirEntry for an fsEntry.
type dirEntry struct {
	entry *fsEntry
}

func (de *dirEntry) Name() string               { return path.Base(de.entry.path) }
func (de *dirEntry) IsDir() bool                { return de.entry.mode&disk.StatTypeMask == disk.StatTypeDir }
func (de *dirEntry) Type() fs.FileMode          { return disk.EroFSModeToGoFileMode(de.entry.mode).Type() }
func (de *dirEntry) Info() (fs.FileInfo, error) { return &writerFileInfo{entry: de.entry}, nil }

// add adds a single entry. Mode and Size come from info; extended metadata
// comes from info.Sys(). Checks Sys() for *builder.Entry first, then
// platform-specific stat types as a fallback for plain fs.FS sources.
func (fsys *Writer) add(p string, info fs.FileInfo) error {
	p = cleanPath(p)
	mode := goModeToUnixMode(info.Mode())
	size := uint64(info.Size())
	typ := mode & disk.StatTypeMask

	be := entryFromSys(info)
	if be == nil {
		be = &builder.Entry{}
	}

	if p == "/" {
		root := fsys.root
		root.mode = mode
		root.uid = be.UID
		root.gid = be.GID
		root.mtime = be.Mtime
		root.mtimeNs = be.MtimeNs
		if be.Nlink > 0 {
			root.nlink = be.Nlink
			root.nlinkSet = true
		}
		root.xattrs = be.Xattrs
		return nil
	}

	fsys.ensureParent(p)

	fe := &fsEntry{
		path:       p,
		mode:       mode,
		uid:        be.UID,
		gid:        be.GID,
		mtime:      be.Mtime,
		mtimeNs:    be.MtimeNs,
		size:       size,
		rdev:       be.Rdev,
		xattrs:     be.Xattrs,
		linkTarget: be.LinkTarget,
		chunks:     be.Chunks,
		contiguous: be.Contiguous,
	}
	if be.Nlink > 0 {
		fe.nlink = be.Nlink
		fe.nlinkSet = true
	}

	// Handle duplicate paths (overwrite semantics).
	if existing, ok := fsys.byPath[p]; ok {
		// Preserve tree linkage, and the counts of references other entries
		// hold on this one, when overwriting.
		savedParent := existing.parent
		savedChildren := existing.children
		savedExtraLinks := existing.extraLinks
		savedShareRefs := existing.shareRefs
		*existing = *fe
		existing.parent = savedParent
		existing.children = savedChildren
		existing.extraLinks = savedExtraLinks
		existing.shareRefs = savedShareRefs
		fe = existing
	} else {
		fsys.addChild(fe)
	}

	if fsys.copyMetadataOnly {
		fe.metadataOnly = true
		// Remap chunk DeviceIDs from source-relative to absolute.
		// For single-device sources, all chunks use DeviceID=1
		// and get mapped to copyDeviceID.
		// For multi-device sources (e.g. EROFS images), chunks have
		// DeviceIDs 1..N that get offset by copyDeviceID-1.
		if fsys.copyDeviceID > 0 {
			offset := fsys.copyDeviceID - 1
			for i := range fe.chunks {
				fe.chunks[i].DeviceID += offset
			}
		}
	}

	// Write regular file data.
	// Skip entirely in metadata-only mode.
	needData := typ == disk.StatTypeReg && size > 0 && be.Data != nil &&
		!fsys.copyMetadataOnly
	if needData {
		// Data is stored locally; clear any source chunk mappings.
		fe.chunks = nil
		fe.contiguous = false
		if fsys.dataFile != nil {
			// Data file mode: copy through File for block-aligned padding and chunk recording.
			f := &File{fs: fsys, entry: fe}
			f.dataStartOff = fsys.dataOff
			fe.dataStartOff = fsys.dataOff
			if _, err := f.ReadFrom(be.Data); err != nil {
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		} else {
			// Spool mode: keep a direct reference to avoid copying.
			fe.directData = be.Data
			fe.fileClosed = true
		}
	} else {
		fe.fileClosed = true
	}

	return nil
}

// checkPath validates that a path hasn't already been registered.
func (fsys *Writer) checkPath(name string) error {
	if err := fsys.checkMutable(); err != nil {
		return err
	}
	if _, ok := fsys.byPath[name]; ok {
		return fmt.Errorf("mkfs: duplicate path %q", name)
	}
	return nil
}

// ensureParent creates implicit parent directories for name.
func (fsys *Writer) ensureParent(name string) {
	dir := path.Dir(name)
	if dir == "/" {
		return
	}
	// Walk up to find existing ancestors.
	var missing []string
	ancestor := fsys.root
	for d := dir; d != "/"; d = path.Dir(d) {
		if e, ok := fsys.byPath[d]; ok {
			ancestor = e
			break
		}
		missing = append(missing, d)
	}
	// Create in top-down order.
	for i := len(missing) - 1; i >= 0; i-- {
		e := fsys.synthesizedDir(missing[i], ancestor)
		fsys.addChild(e)
		ancestor = e
	}
}

// synthesizedDir builds a directory entry the caller never described, giving it
// the attributes the synthetic-directory hook chooses, or the default 0755
// root-owned ones.
func (fsys *Writer) synthesizedDir(p string, ancestor *fsEntry) *fsEntry {
	e := &fsEntry{
		path:        p,
		mode:        disk.StatTypeDir | 0o755,
		synthesized: true,
	}
	if fsys.syntheticDirMeta == nil {
		return e
	}
	from := fsys.root
	if ancestor != nil {
		from = ancestor
	}
	if meta, ok := fsys.syntheticDirMeta(p, ancestorMetadata(from)); ok {
		e.mode = disk.StatTypeDir | (meta.Mode & 0o7777)
		e.uid = meta.UID
		e.gid = meta.GID
		e.mtime = meta.Mtime
		e.mtimeNs = meta.MtimeNs
		e.xattrs = maps.Clone(meta.Xattrs)
	}
	return e
}

// ancestorMetadata describes an existing directory for the synthetic-directory
// hook, mirroring what mkfs.erofs inherits from the nearest ancestor.
func ancestorMetadata(e *fsEntry) SyntheticDirMetadata {
	return SyntheticDirMetadata{
		Mode:    e.mode & 0o7777,
		UID:     e.uid,
		GID:     e.gid,
		Mtime:   e.mtime,
		MtimeNs: e.mtimeNs,
		Xattrs:  e.xattrs,
	}
}

// addChild registers an entry in the tree and byPath map.
// The entry's parent is resolved from its path.
func (fsys *Writer) addChild(e *fsEntry) {
	parent := fsys.byPath[path.Dir(e.path)]
	if parent == nil {
		parent = fsys.root
	}
	e.parent = parent
	parent.children = append(parent.children, e)
	fsys.byPath[e.path] = e
}

// remove marks an entry and all its descendants as removed.
// Used by Merge to process whiteout deletions.
func (fsys *Writer) remove(p string) {
	p = cleanPath(p)
	e, ok := fsys.byPath[p]
	if !ok {
		return
	}
	e.removed = true
	delete(fsys.byPath, p)
	if e.mode&disk.StatTypeMask == disk.StatTypeDir {
		fsys.removeSubtree(e)
	}
}

// removeChildren marks all descendants of a directory as removed.
// The directory itself is not removed.
func (fsys *Writer) removeChildren(dir string) {
	dir = cleanPath(dir)
	e, ok := fsys.byPath[dir]
	if !ok {
		return
	}
	fsys.removeSubtree(e)
}

// removeSubtree recursively marks all descendants of e as removed.
func (fsys *Writer) removeSubtree(e *fsEntry) {
	for _, c := range e.children {
		if !c.removed {
			c.removed = true
			delete(fsys.byPath, c.path)
			if c.mode&disk.StatTypeMask == disk.StatTypeDir {
				fsys.removeSubtree(c)
			}
		}
	}
}

// buildErofsTree converts the fsEntry tree into an erofsEntry tree via BFS.
// Children are sorted for deterministic output. The Writer is consumed.
func (fsys *Writer) buildErofsTree() (*erofsEntry, error) {
	type pair struct {
		fs *fsEntry
		er *erofsEntry
	}

	// Hardlinks and shared payloads point at other entries, which BFS may not
	// have converted yet, so the pointers are wired up in a second pass.
	converted := make(map[*fsEntry]*erofsEntry, len(fsys.byPath))
	rootEr := fsys.fsToErofs(fsys.root)
	converted[fsys.root] = rootEr
	queue := []pair{{fsys.root, rootEr}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		// Count child directories for nlink.
		var childDirs uint32
		for _, c := range cur.fs.children {
			if !c.removed && c.mode&disk.StatTypeMask == disk.StatTypeDir {
				childDirs++
			}
		}
		if !cur.fs.nlinkSet && cur.fs.mode&disk.StatTypeMask == disk.StatTypeDir {
			cur.er.nlink = 2 + childDirs
		}

		// Convert and enqueue children.
		if len(cur.fs.children) > 0 {
			cur.er.children = make([]*erofsEntry, 0, len(cur.fs.children))
		}
		for _, c := range cur.fs.children {
			if c.removed {
				continue
			}
			ent := fsys.fsToErofs(c)
			converted[c] = ent
			cur.er.children = append(cur.er.children, ent)
			if c.mode&disk.StatTypeMask == disk.StatTypeDir {
				queue = append(queue, pair{c, ent})
			}
		}

		// Sort children for deterministic output.
		sort.Slice(cur.er.children, func(i, j int) bool {
			return cur.er.children[i].name < cur.er.children[j].name
		})
	}

	for fe, ee := range converted {
		if fe.hardlinkTo != nil {
			target, ok := converted[fe.hardlinkTo]
			if !ok {
				return nil, fmt.Errorf("mkfs: hardlink %q points at removed entry %q", fe.path, fe.hardlinkTo.path)
			}
			ee.hardlinkTo = target
		}
		if fe.shareSrc != nil {
			src, ok := converted[fe.shareSrc]
			if !ok {
				return nil, fmt.Errorf("mkfs: %q shares data with removed entry %q", fe.path, fe.shareSrc.path)
			}
			ee.shareSrc = src
			src.isShareSrc = true
		}
	}
	return rootEr, nil
}

// fsToErofs converts a single fsEntry to an erofsEntry, resolving data readers.
func (fsys *Writer) fsToErofs(e *fsEntry) *erofsEntry {
	var nlink uint32
	switch {
	case e.nlinkSet:
		nlink = e.nlink
	case e.mode&disk.StatTypeMask == disk.StatTypeDir:
		nlink = 2 // adjusted by buildErofsTree
	default:
		nlink = 1 + e.extraLinks
	}

	var data io.Reader
	if fsys.dataFile == nil && len(e.chunks) == 0 && !e.metadataOnly &&
		e.hardlinkTo == nil && e.shareSrc == nil &&
		e.mode&disk.StatTypeMask == disk.StatTypeReg && e.size > 0 {
		if e.directData != nil {
			data = e.directData
		} else if fsys.spool != nil {
			data = io.NewSectionReader(fsys.spool, e.spoolOff, int64(e.size))
		}
	}

	return &erofsEntry{
		mode:          e.mode,
		uid:           e.uid,
		gid:           e.gid,
		mtime:         e.mtime,
		mtimeNs:       e.mtimeNs,
		nlink:         nlink,
		size:          e.size,
		rdev:          e.rdev,
		name:          path.Base(e.path),
		path:          e.path,
		symTarget:     e.linkTarget,
		chunks:        e.chunks,
		contiguous:    e.contiguous,
		metadataOnly:  e.metadataOnly,
		data:          data,
		xattrs:        e.xattrs,
		token:         e.token,
		erofsFileType: modeToFileType(e.mode),
	}
}

// setBlockSize sets the image block size. If already set to a different
// value, it returns an error. Safe to call multiple times with the same value.
func (fsys *Writer) setBlockSize(n int) error {
	if n < minBlockSize || n > maxBlockSize {
		return fmt.Errorf("mkfs: invalid block size %d: must be between %d and %d", n, minBlockSize, maxBlockSize)
	}
	if bits.OnesCount(uint(n)) != 1 {
		return fmt.Errorf("mkfs: invalid block size %d: must be a power of two", n)
	}
	if fsys.blockSize == 0 {
		fsys.blockSize = n
		return nil
	}
	if fsys.blockSize != n {
		return fmt.Errorf("mkfs: block size conflict: already %d, requested %d", fsys.blockSize, n)
	}
	return nil
}

// resolveBlockSize returns the block size, defaulting to 4096 if unset.
func (fsys *Writer) resolveBlockSize() int {
	if fsys.blockSize == 0 {
		fsys.blockSize = defaultBlockSize
	}
	return fsys.blockSize
}

// copyBuf returns a shared 32KB buffer for io.Copy operations.
func (fsys *Writer) copyBuf() []byte {
	if fsys.cpBuf == nil {
		fsys.cpBuf = make([]byte, 32*1024)
	}
	return fsys.cpBuf
}

// zeroPad returns a shared zero buffer sized to the resolved block size.
func (fsys *Writer) zeroPad() []byte {
	if fsys.padBuf == nil {
		fsys.padBuf = make([]byte, fsys.resolveBlockSize())
	}
	return fsys.padBuf
}

// chunksFromRanges converts DataRange entries into internal chunk entries.
// fileSize is the logical size of the file; the sum of all range Sizes must
// equal fileSize exactly, or an error is returned.
//
// The block size used is the Writer's resolved block size. DataRange.Device
// values are offset by 1 to produce chunk DeviceIDs: DataRange Device 0
// becomes chunk DeviceID 1 (the first extra device), matching the EROFS
// convention where DeviceID 0 is the primary image.
//
// Validation rules:
//   - sum(Size) == fileSize; a mismatch is rejected.
//   - r.Size > 0 for every entry.
//   - Hole entries (Offset == -1) emit [builder.NullPhysicalBlock] chunks.
//     Hole Size must be block-aligned for non-final entries; the final entry
//     may end mid-block to match the file tail.
//   - For data entries: r.Offset >= 0 and block-aligned; r.Device == 0.
//   - For non-final data entries: r.Size must be a multiple of blockSize.
//     The final entry may have a partial last block to match the file tail.
func (fsys *Writer) chunksFromRanges(ranges []DataRange, fileSize int64) ([]builder.Chunk, error) {
	blockSize := uint64(fsys.resolveBlockSize())

	// Validate total coverage first.
	var total int64
	for _, r := range ranges {
		total += r.Size
	}
	if total != fileSize {
		return nil, fmt.Errorf("DataRange total size %d does not match file size %d", total, fileSize)
	}

	last := len(ranges) - 1
	var chunks []builder.Chunk
	for i, r := range ranges {
		if r.Size <= 0 {
			return nil, fmt.Errorf("DataRange[%d]: non-positive Size %d", i, r.Size)
		}
		// Non-final entries must be block-aligned in size; the final entry may
		// end mid-block to match the file tail.
		if i < last && uint64(r.Size)%blockSize != 0 {
			return nil, fmt.Errorf("DataRange[%d]: non-final Size %d is not block-aligned (block size %d)", i, r.Size, blockSize)
		}
		if r.Offset == holeOffset {
			// Hole: emit NullPhysicalBlock chunks covering the hole span.
			totalBlocks := (uint64(r.Size) + blockSize - 1) / blockSize
			for totalBlocks > 0 {
				count := totalBlocks
				if count > 65535 {
					count = 65535
				}
				chunks = append(chunks, builder.Chunk{
					PhysicalBlock: builder.NullPhysicalBlock,
					Count:         uint16(count),
				})
				totalBlocks -= count
			}
			continue
		}
		if r.Offset < 0 {
			return nil, fmt.Errorf("DataRange[%d]: negative Offset %d", i, r.Offset)
		}
		if uint64(r.Offset)%blockSize != 0 {
			return nil, fmt.Errorf("DataRange[%d]: Offset %d is not block-aligned (block size %d)", i, r.Offset, blockSize)
		}
		// Non-EROFS sources register exactly one device via DeviceBlocks();
		// only Device=0 is valid. Device=0xFFFF would also wrap deviceID to 0
		// (the primary image), producing an invalid mapping.
		if r.Device != 0 {
			return nil, fmt.Errorf("DataRange[%d]: Device %d out of range (source declared one device, only Device=0 is valid)", i, r.Device)
		}
		deviceID := r.Device + 1
		startBlock := uint64(r.Offset) / blockSize
		totalBlocks := (uint64(r.Size) + blockSize - 1) / blockSize
		for totalBlocks > 0 {
			count := totalBlocks
			if count > 65535 {
				count = 65535
			}
			chunks = append(chunks, builder.Chunk{
				PhysicalBlock: startBlock,
				Count:         uint16(count),
				DeviceID:      deviceID,
			})
			startBlock += count
			totalBlocks -= count
		}
	}
	return chunks, nil
}

// ensureSpool lazily creates the spool temp file.
func (fsys *Writer) ensureSpool() error {
	if fsys.spool != nil {
		return nil
	}
	tmp, err := os.CreateTemp(fsys.tempDir, "erofs-mkfs-*")
	if err != nil {
		return fmt.Errorf("mkfs: create spool: %w", err)
	}
	_ = os.Remove(tmp.Name()) // unlink immediately; fd keeps data accessible
	fsys.spool = tmp
	return nil
}

func (fsys *Writer) lookup(name string) (*fsEntry, error) {
	name = cleanPath(name)
	e, ok := fsys.byPath[name]
	if !ok {
		return nil, fmt.Errorf("mkfs: path not found %q", name)
	}
	return e, nil
}

// closeDataFile pads the data file to a block boundary and records chunks.
func (f *File) closeDataFile() error {
	if f.written == 0 {
		return nil
	}

	// Pad to block boundary.
	bs := int64(f.fs.resolveBlockSize())
	rem := f.fs.dataOff % bs
	if rem != 0 {
		padSize := bs - rem
		n, err := f.fs.dataFile.Write(f.fs.zeroPad()[:padSize])
		f.fs.dataOff += int64(n)
		if err != nil {
			return fmt.Errorf("mkfs: pad data file: %w", err)
		}
	}

	// Compute chunks from the start offset and written bytes.
	startBlock := uint64(f.dataStartOff) / uint64(f.fs.resolveBlockSize())
	totalBlocks := (uint64(f.written) + uint64(f.fs.resolveBlockSize()) - 1) / uint64(f.fs.resolveBlockSize())

	for totalBlocks > 0 {
		count := totalBlocks
		if count > 65535 {
			count = 65535
		}
		f.entry.chunks = append(f.entry.chunks, builder.Chunk{
			PhysicalBlock: startBlock,
			Count:         uint16(count),
			DeviceID:      1,
		})
		startBlock += count
		totalBlocks -= count
	}

	return nil
}

// --- Constants ---

// Unix file type bits, as stored in an EROFS inode's i_mode. They match the
// S_IF* values from <sys/stat.h>, which the on-disk format uses verbatim, and
// are what [Writer.Mknod] expects in its mode argument.
const (
	ModeFifo     uint16 = 0o010000
	ModeChardev  uint16 = 0o020000
	ModeDir      uint16 = 0o040000
	ModeBlockdev uint16 = 0o060000
	ModeRegular  uint16 = 0o100000
	ModeSymlink  uint16 = 0o120000
	ModeSocket   uint16 = 0o140000

	// ModeTypeMask selects the file type bits of a mode.
	ModeTypeMask uint16 = 0o170000
)

const (
	minBlockSize     = 512
	defaultBlockSize = 4096
	nullAddr         = 0xFFFFFFFF // marks a hole/sparse chunk

	// Overlay whiteout markers (AUFS convention used by OCI layers).
	whiteoutPrefix = ".wh."
	opaqueWhiteout = ".wh..wh..opq"
)

// blkBits returns log2(blockSize).
func blkBits(blockSize int) uint8 {
	return uint8(bits.TrailingZeros(uint(blockSize)))
}
