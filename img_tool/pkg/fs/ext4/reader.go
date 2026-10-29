package ext4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"time"
)

// Reader reads the images written by this package: a read-only view of the
// directory tree, the metadata of every inode and the location of file
// content. It understands the features the writer uses (extents, linear
// directories, extended attributes in the inode or in a block) and rejects
// everything else. Checksums are not verified.
//
// The reader works on any [io.ReaderAt], including one in which the content of
// files reads as zeros (the sparse view of a compact stream): it only reads
// the metadata, unless asked for file content with [Reader.Content].
type Reader struct {
	r         io.ReaderAt
	bs        int64
	ipg       uint32
	groups    int
	inodeSize int
	gdt       []byte
	descSize  int
}

// Inode is the metadata of a file, directory or other object of an image.
type Inode struct {
	Number uint32
	// Mode holds the type and the permission bits (setuid, setgid, sticky).
	Mode     fs.FileMode
	UID, GID uint32
	Size     int64
	Links    uint32
	Atime    time.Time
	Ctime    time.Time
	Mtime    time.Time
	Major    uint32 // device nodes
	Minor    uint32
	Target   string // symlinks
	Xattrs   map[string]string
	Extents  []Extent
}

// Extent is a range of the image that holds part of the content of a file, in
// file order.
type Extent struct {
	// Offset is the position in the image, in bytes.
	Offset int64
	// Length is the number of bytes, a multiple of the block size.
	Length int64
}

// OpenReader opens the image in r.
func OpenReader(r io.ReaderAt) (*Reader, error) {
	sb := make([]byte, 1024)
	if err := readFull(r, sb, 1024); err != nil {
		return nil, fmt.Errorf("ext4: reading the superblock: %w", err)
	}
	if le.Uint16(sb[0x38:]) != 0xEF53 {
		return nil, errors.New("ext4: not an ext4 image (bad magic)")
	}
	if incompat := le.Uint32(sb[0x60:]); incompat&^(featIncompatFiletype|featIncompatExtents|featIncompatFlexBG) != 0 {
		return nil, fmt.Errorf("ext4: unsupported incompatible features %#x", incompat)
	}
	logBlock := le.Uint32(sb[0x18:])
	if logBlock > 6 {
		return nil, fmt.Errorf("ext4: invalid block size shift %d", logBlock)
	}
	rd := &Reader{
		r:         r,
		bs:        1024 << logBlock,
		ipg:       le.Uint32(sb[0x28:]),
		inodeSize: int(le.Uint16(sb[0x58:])),
		descSize:  descSize,
	}
	if rd.ipg == 0 || rd.inodeSize < 128 || rd.inodeSize > int(rd.bs) {
		return nil, errors.New("ext4: invalid inode geometry")
	}
	blocks := uint64(le.Uint32(sb[0x04:]))
	first := uint64(le.Uint32(sb[0x14:]))
	bpg := uint64(le.Uint32(sb[0x20:]))
	if bpg == 0 || blocks < first {
		return nil, errors.New("ext4: invalid block geometry")
	}
	rd.groups = int((blocks - first + bpg - 1) / bpg)
	gdtBlocks := (int64(rd.groups*rd.descSize) + rd.bs - 1) / rd.bs
	rd.gdt = make([]byte, gdtBlocks*rd.bs)
	if err := readFull(r, rd.gdt, (int64(first)+1)*rd.bs); err != nil {
		return nil, fmt.Errorf("ext4: reading the group descriptors: %w", err)
	}
	return rd, nil
}

func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	return unexpected(err)
}

// Inode reads the inode with the given number.
func (rd *Reader) Inode(ino uint32) (*Inode, error) {
	if ino == 0 || uint64(ino) > uint64(rd.ipg)*uint64(rd.groups) {
		return nil, fmt.Errorf("ext4: inode %d out of range", ino)
	}
	g := int((ino - 1) / rd.ipg)
	idx := (ino - 1) % rd.ipg
	table := int64(le.Uint32(rd.gdt[g*rd.descSize+8:]))
	rec := make([]byte, rd.inodeSize)
	if err := readFull(rd.r, rec, table*rd.bs+int64(idx)*int64(rd.inodeSize)); err != nil {
		return nil, fmt.Errorf("ext4: reading inode %d: %w", ino, err)
	}

	mode := le.Uint16(rec[0:])
	in := &Inode{
		Number: ino,
		UID:    uint32(le.Uint16(rec[0x02:])) | uint32(le.Uint16(rec[0x78:]))<<16,
		GID:    uint32(le.Uint16(rec[0x18:])) | uint32(le.Uint16(rec[0x7A:]))<<16,
		Size:   int64(le.Uint32(rec[0x04:])) | int64(le.Uint32(rec[0x6C:]))<<32,
		Links:  uint32(le.Uint16(rec[0x1A:])),
		Mode:   fileMode(mode),
	}
	if in.Size < 0 {
		return nil, fmt.Errorf("ext4: inode %d: invalid size", ino)
	}
	extra := 0
	if rd.inodeSize > 128 {
		extra = int(le.Uint16(rec[0x80:]))
	}
	// A field is only present if the extra inode fields reach past its end.
	extraField := func(off int) uint32 {
		if off+4 <= 128+extra && off+4 <= len(rec) {
			return le.Uint32(rec[off:])
		}
		return 0
	}
	in.Atime = decodeTime(le.Uint32(rec[0x08:]), extraField(0x8C))
	in.Ctime = decodeTime(le.Uint32(rec[0x0C:]), extraField(0x84))
	in.Mtime = decodeTime(le.Uint32(rec[0x10:]), extraField(0x88))

	flags := le.Uint32(rec[0x20:])
	iblock := rec[0x28 : 0x28+60]
	switch {
	case in.Mode&fs.ModeDevice != 0:
		if v := le.Uint32(iblock[0:]); v != 0 {
			in.Major, in.Minor = v>>8&0xff, v&0xff
		} else {
			v = le.Uint32(iblock[4:])
			in.Major, in.Minor = v>>8&0xfff, v&0xff|v>>12&0xfff00
		}
	case in.Mode&fs.ModeSymlink != 0 && flags&flagExtents == 0:
		if in.Size > 60 {
			return nil, fmt.Errorf("ext4: inode %d: fast symlink of %d bytes", ino, in.Size)
		}
		in.Target = string(iblock[:in.Size])
	case flags&flagExtents != 0:
		if err := rd.readExtents(in, iblock, 0); err != nil {
			return nil, err
		}
	}
	if in.Mode&fs.ModeSymlink != 0 && flags&flagExtents != 0 {
		target := make([]byte, in.Size)
		if _, err := io.ReadFull(rd.Content(in), target); err != nil {
			return nil, fmt.Errorf("ext4: inode %d: reading the symlink target: %w", ino, unexpected(err))
		}
		in.Target = string(target)
	}

	if err := rd.readXattrs(in, rec, extra); err != nil {
		return nil, err
	}
	return in, nil
}

func fileMode(mode uint16) fs.FileMode {
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
	switch mode & 0xF000 {
	case 0x4000:
		m |= fs.ModeDir
	case 0xA000:
		m |= fs.ModeSymlink
	case 0x2000:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case 0x6000:
		m |= fs.ModeDevice
	case 0x1000:
		m |= fs.ModeNamedPipe
	case 0xC000:
		m |= fs.ModeSocket
	}
	return m
}

// decodeTime is the inverse of the writer's timestamp encoding: 32 bits of
// seconds (signed), then two more bits of seconds and 30 bits of nanoseconds
// in the extra field.
func decodeTime(sec, extra uint32) time.Time {
	s := int64(int32(sec)) + int64(extra&3)<<32
	return time.Unix(s, int64(extra>>2))
}

// readExtents appends the extents below an extent tree node to in.
func (rd *Reader) readExtents(in *Inode, node []byte, level int) error {
	if level > 5 || len(node) < 12 || le.Uint16(node[0:]) != 0xF30A {
		return fmt.Errorf("ext4: inode %d: bad extent tree", in.Number)
	}
	entries, depth := int(le.Uint16(node[2:])), int(le.Uint16(node[6:]))
	if 12+12*entries > len(node) {
		return fmt.Errorf("ext4: inode %d: bad extent tree", in.Number)
	}
	for i := 0; i < entries; i++ {
		e := node[12+12*i:]
		if depth == 0 {
			length := int64(le.Uint16(e[4:]))
			if length > maxExtentLen {
				return fmt.Errorf("ext4: inode %d: uninitialized extents are not supported", in.Number)
			}
			phys := int64(le.Uint16(e[6:]))<<32 | int64(le.Uint32(e[8:]))
			logical := int64(le.Uint32(e[0:]))
			// The writer never leaves holes, so extents follow each other.
			var have int64
			for _, x := range in.Extents {
				have += x.Length / rd.bs
			}
			if logical != have {
				return fmt.Errorf("ext4: inode %d: sparse files are not supported", in.Number)
			}
			in.Extents = append(in.Extents, Extent{Offset: phys * rd.bs, Length: length * rd.bs})
			continue
		}
		child := int64(le.Uint16(e[8:]))<<32 | int64(le.Uint32(e[4:]))
		block := make([]byte, rd.bs)
		if err := readFull(rd.r, block, child*rd.bs); err != nil {
			return fmt.Errorf("ext4: inode %d: reading an extent block: %w", in.Number, err)
		}
		if err := rd.readExtents(in, block, level+1); err != nil {
			return err
		}
	}
	return nil
}

// Content returns a reader for the content of a regular file, a directory or a
// long symlink.
func (rd *Reader) Content(in *Inode) io.Reader {
	readers := make([]io.Reader, 0, len(in.Extents))
	remaining := in.Size
	for _, e := range in.Extents {
		n := min(e.Length, remaining)
		if n <= 0 {
			break
		}
		readers = append(readers, io.NewSectionReader(rd.r, e.Offset, n))
		remaining -= n
	}
	return io.MultiReader(readers...)
}

var xattrPrefixNames = map[uint8]string{1: "user.", 2: "system.posix_acl_access", 3: "system.posix_acl_default", 4: "trusted.", 6: "security.", 7: "system."}

func (rd *Reader) readXattrs(in *Inode, rec []byte, extra int) error {
	in.Xattrs = map[string]string{}
	if ibody := 128 + extra; extra > 0 && ibody+4 <= len(rec) && le.Uint32(rec[ibody:]) == xattrMagic {
		if err := parseXattrEntries(in.Xattrs, rec[ibody+4:], 0, 0); err != nil {
			return fmt.Errorf("ext4: inode %d: %w", in.Number, err)
		}
	}
	if blk := int64(le.Uint32(rec[0x68:])) | int64(le.Uint16(rec[0x76:]))<<32; blk != 0 {
		block := make([]byte, rd.bs)
		if err := readFull(rd.r, block, blk*rd.bs); err != nil {
			return fmt.Errorf("ext4: inode %d: reading the attribute block: %w", in.Number, err)
		}
		if le.Uint32(block[0:]) != xattrMagic {
			return fmt.Errorf("ext4: inode %d: bad attribute block", in.Number)
		}
		if err := parseXattrEntries(in.Xattrs, block, xattrBlockHeader, 0); err != nil {
			return fmt.Errorf("ext4: inode %d: %w", in.Number, err)
		}
	}
	if len(in.Xattrs) == 0 {
		in.Xattrs = nil
	}
	return nil
}

// parseXattrEntries decodes the attributes of an attribute area. Value offsets
// are relative to base.
func parseXattrEntries(out map[string]string, area []byte, entries, base int) error {
	for pos := entries; pos+4 <= len(area) && le.Uint32(area[pos:]) != 0; {
		if pos+xattrEntryHeader > len(area) {
			return errors.New("truncated extended attribute")
		}
		nameLen := int(area[pos])
		idx := area[pos+1]
		offs := int(le.Uint16(area[pos+2:]))
		size := int(le.Uint32(area[pos+8:]))
		prefix, ok := xattrPrefixNames[idx]
		if !ok {
			return fmt.Errorf("extended attribute with unknown name index %d", idx)
		}
		if pos+xattrEntryHeader+nameLen > len(area) || base+offs+size > len(area) {
			return errors.New("extended attribute out of bounds")
		}
		name := prefix + string(area[pos+xattrEntryHeader:pos+xattrEntryHeader+nameLen])
		value := area[base+offs : base+offs+size]
		if idx == 2 || idx == 3 {
			var err error
			if value, err = posixACLFromDisk(value); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		out[name] = string(value)
		pos += xattrEntryHeader + pad4(nameLen)
	}
	return nil
}

// posixACLFromDisk is the inverse of posixACLToDisk.
func posixACLFromDisk(in []byte) ([]byte, error) {
	const (
		user  = 0x02
		group = 0x08
	)
	if len(in) < 4 || le.Uint32(in) != 1 {
		return nil, errors.New("malformed posix ACL")
	}
	out := binary.LittleEndian.AppendUint32(nil, 2)
	for rest := in[4:]; len(rest) > 0; {
		if len(rest) < 4 {
			return nil, errors.New("malformed posix ACL")
		}
		tag, perm := le.Uint16(rest), le.Uint16(rest[2:])
		rest = rest[4:]
		var id uint32 = 0xFFFFFFFF
		if tag == user || tag == group {
			if len(rest) < 4 {
				return nil, errors.New("malformed posix ACL")
			}
			id = le.Uint32(rest)
			rest = rest[4:]
		}
		out = binary.LittleEndian.AppendUint16(out, tag)
		out = binary.LittleEndian.AppendUint16(out, perm)
		out = binary.LittleEndian.AppendUint32(out, id)
	}
	return out, nil
}

// Walk calls fn for every directory entry of the image, depth first, with the
// entries of a directory in name order. The root is visited first, as ".".
// Files with several hard links are visited once per link.
func (rd *Reader) Walk(fn func(path string, in *Inode) error) error {
	root, err := rd.Inode(rootIno)
	if err != nil {
		return err
	}
	if !root.Mode.IsDir() {
		return errors.New("ext4: the root is not a directory")
	}
	return rd.walk(".", root, 0, fn)
}

func (rd *Reader) walk(p string, in *Inode, depth int, fn func(string, *Inode) error) error {
	if depth > 4096 {
		return errors.New("ext4: directories nested too deeply")
	}
	if err := fn(p, in); err != nil {
		return err
	}
	if !in.Mode.IsDir() {
		return nil
	}
	ents, err := rd.readDir(in)
	if err != nil {
		return err
	}
	for _, e := range ents {
		child, err := rd.Inode(e.ino)
		if err != nil {
			return err
		}
		name := e.name
		if p != "." {
			name = p + "/" + name
		}
		if err := rd.walk(name, child, depth+1, fn); err != nil {
			return err
		}
	}
	return nil
}

// readDir returns the entries of a directory, without "." and "..", sorted by
// name.
func (rd *Reader) readDir(in *Inode) ([]dirent, error) {
	data := make([]byte, in.Size)
	if _, err := io.ReadFull(rd.Content(in), data); err != nil {
		return nil, fmt.Errorf("ext4: inode %d: reading the directory: %w", in.Number, unexpected(err))
	}
	var ents []dirent
	for pos := 0; pos+8 <= len(data); {
		ino := le.Uint32(data[pos:])
		recLen := int(le.Uint16(data[pos+4:]))
		nameLen := int(data[pos+6])
		if recLen < 8 || recLen%4 != 0 || pos+recLen > len(data) || 8+nameLen > recLen {
			return nil, fmt.Errorf("ext4: inode %d: corrupt directory entry at %d", in.Number, pos)
		}
		name := string(data[pos+8 : pos+8+nameLen])
		if ino != 0 && name != "." && name != ".." {
			ents = append(ents, dirent{name: name, ino: ino, ftype: data[pos+7]})
		}
		pos += recLen
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].name < ents[j].name })
	return ents, nil
}
