package iso9660

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
)

// Entry is a file, directory or other object of an image, as read by [Reader].
type Entry struct {
	// Path is the slash-separated path without leading slash. The root
	// directory has the empty path.
	Path string
	// Mode holds the type and permission bits. Without Rock Ridge an image
	// stores none, so files are 0444 and directories 0555.
	Mode fs.FileMode
	// Uid and Gid are the owner, zero without Rock Ridge.
	Uid, Gid int
	// Mtime, Atime and Ctime are the modification, access and attribute change
	// time. Without Rock Ridge all three are the recording time of the
	// directory record.
	Mtime, Atime, Ctime time.Time
	// Nlink is the link count Rock Ridge records, or one.
	Nlink int
	// Target is the target of a symbolic link.
	Target string
	// Major and Minor are the device numbers of a device node.
	Major, Minor uint32
	// Size is the size of the content of a regular file.
	Size int64
	// Offset is the position of the content of a regular file in the image, or
	// zero for a file without content. The writer stores each file in one
	// contiguous, block-aligned extent.
	Offset int64
}

// Reader reads the images this package writes: a read-only view of the
// directory tree and the location of file content. It walks the primary
// hierarchy and prefers the Rock Ridge metadata where the image carries it,
// which is what a Linux reader does.
//
// The reader works on any [io.ReaderAt], including one in which the content of
// files reads as zeros (the sparse view of a compact stream): it only reads the
// metadata, unless asked for file content with [Reader.Content].
type Reader struct {
	r io.ReaderAt
	// rootLBA and rootLength locate the root directory extent.
	rootLBA, rootLength uint32
	// rockRidge reports whether the root directory announced the extension.
	rockRidge bool
	// Blocks is the number of logical blocks the volume declares.
	Blocks uint32
	// VolumeID is the volume identifier of the primary volume descriptor.
	VolumeID string
}

// RockRidge reports whether the image carries the Rock Ridge extension, whose
// metadata the reader then prefers.
func (rd *Reader) RockRidge() bool { return rd.rockRidge }

// OpenReader opens the image in r.
func OpenReader(r io.ReaderAt) (*Reader, error) {
	pvd := make([]byte, BlockSize)
	if err := readFull(r, pvd, systemAreaBlocks*BlockSize); err != nil {
		return nil, fmt.Errorf("iso9660: reading the primary volume descriptor: %w", err)
	}
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		return nil, errors.New("iso9660: not an ISO 9660 image")
	}
	blockSize := binary.LittleEndian.Uint16(pvd[128:])
	if blockSize != BlockSize {
		return nil, fmt.Errorf("iso9660: logical block size %d is not supported", blockSize)
	}
	rd := &Reader{
		r:          r,
		Blocks:     binary.LittleEndian.Uint32(pvd[80:]),
		rootLBA:    binary.LittleEndian.Uint32(pvd[158:]),
		rootLength: binary.LittleEndian.Uint32(pvd[166:]),
		VolumeID:   strings.TrimRight(string(pvd[40:72]), " "),
	}
	if rd.rootLength == 0 || rd.rootLength%BlockSize != 0 {
		return nil, fmt.Errorf("iso9660: the root directory extent of %d bytes is not a whole number of blocks", rd.rootLength)
	}
	var err error
	if rd.rockRidge, err = rd.detectRockRidge(); err != nil {
		return nil, err
	}
	return rd, nil
}

// detectRockRidge looks for the SUSP indicator in the "." record of the root
// directory, which is where a reader is told that the extension is in use.
func (rd *Reader) detectRockRidge() (bool, error) {
	block := make([]byte, BlockSize)
	if err := readFull(rd.r, block, int64(rd.rootLBA)*BlockSize); err != nil {
		return false, err
	}
	if len(block) < 34 || block[0] < 34 {
		return false, errors.New("iso9660: the root directory has no \".\" record")
	}
	rec := block[:block[0]]
	for _, entry := range suspOf(rec) {
		if entry.signature == "SP" && len(entry.payload) >= 2 && entry.payload[0] == 0xBE && entry.payload[1] == 0xEF {
			return true, nil
		}
	}
	return false, nil
}

// readFull reads len(p) bytes at off. Bytes past the end of r read as zeros,
// like in a sparse file.
func readFull(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	clear(p[n:])
	return nil
}

// Content returns a reader for the content of a regular file.
func (rd *Reader) Content(e *Entry) io.Reader {
	return io.NewSectionReader(rd.r, e.Offset, e.Size)
}

// Walk calls fn for every file and directory of the image, starting with the
// root. A directory comes before its content, and the entries of a directory
// are visited in the order of the directory.
func (rd *Reader) Walk(fn func(*Entry) error) error {
	root := &Entry{Mode: fs.ModeDir | 0o555, Nlink: 1}
	if err := rd.readDotMetadata(rd.rootLBA, root); err != nil {
		return err
	}
	if err := fn(root); err != nil {
		return err
	}
	return rd.walkDir(rd.rootLBA, rd.rootLength, "", 0, fn)
}

// readDotMetadata fills in what the "." record of a directory says about the
// directory itself.
func (rd *Reader) readDotMetadata(lba uint32, e *Entry) error {
	block := make([]byte, BlockSize)
	if err := readFull(rd.r, block, int64(lba)*BlockSize); err != nil {
		return err
	}
	if block[0] < 34 {
		return errors.New("iso9660: a directory has no \".\" record")
	}
	rec := block[:block[0]]
	e.Mtime = decodeShortTime(rec[18:25])
	e.Atime, e.Ctime = e.Mtime, e.Mtime
	if rd.rockRidge {
		rr, err := rd.rockRidgeOf(rec)
		if err != nil {
			return err
		}
		rr.applyTo(e)
	}
	return nil
}

func (rd *Reader) walkDir(lba, length uint32, dirPath string, depth int, fn func(*Entry) error) error {
	if depth > 128 {
		return errors.New("iso9660: directories are nested too deep")
	}
	data := make([]byte, length)
	if err := readFull(rd.r, data, int64(lba)*BlockSize); err != nil {
		return err
	}

	// pending collects the records of a file that spans several extents, which
	// are consecutive, so one slot is enough.
	var pending *Entry
	var pendingLBA uint32
	for pos := 0; pos < len(data); {
		if pos%BlockSize > BlockSize-34 || data[pos] == 0 {
			// The rest of the block is padding.
			pos += BlockSize - pos%BlockSize
			continue
		}
		size := int(data[pos])
		if size < 34 || pos+size > len(data) {
			return fmt.Errorf("iso9660: directory %q: a record of %d bytes at offset %d does not fit", dirPath, size, pos)
		}
		rec := data[pos : pos+size]
		pos += size

		idLen := int(rec[32])
		if 33+idLen > size {
			return fmt.Errorf("iso9660: directory %q: an identifier of %d bytes does not fit in its record", dirPath, idLen)
		}
		id := rec[33 : 33+idLen]
		if idLen == 1 && (id[0] == 0 || id[0] == 1) {
			continue // "." and ".."
		}
		flags := rec[25]
		isDir := flags&flagDirectory != 0

		name := decodeIdentifier(id, isDir)
		e := &Entry{Nlink: 1}
		e.Mtime = decodeShortTime(rec[18:25])
		e.Atime, e.Ctime = e.Mtime, e.Mtime
		if isDir {
			e.Mode = fs.ModeDir | 0o555
		} else {
			e.Mode = 0o444
			e.Size = int64(binary.LittleEndian.Uint32(rec[10:]))
		}
		if rd.rockRidge {
			rr, err := rd.rockRidgeOf(rec)
			if err != nil {
				return err
			}
			if rr.name != "" {
				name = rr.name
			}
			rr.applyTo(e)
		}
		e.Path = strings.TrimPrefix(dirPath+"/"+name, "/")
		childLBA := binary.LittleEndian.Uint32(rec[2:])
		childLength := binary.LittleEndian.Uint32(rec[10:])

		// A file larger than one extent is recorded in several consecutive
		// records that share an identifier; all but the last are marked as
		// continuing. They describe one file, so their lengths are added up and
		// the location of the first extent is the location of the file -- the
		// writer stores a file as one contiguous run.
		if !isDir && flags&flagMultiExtent != 0 {
			if pending == nil {
				pending, pendingLBA = e, childLBA
				pending.Size = 0
			}
			pending.Size += int64(childLength)
			continue
		}
		if pending != nil {
			if pending.Path != e.Path {
				return fmt.Errorf("iso9660: directory %q: the records of %q are interrupted by %q", dirPath, pending.Path, e.Path)
			}
			pending.Size += e.Size
			e, childLBA = pending, pendingLBA
			pending = nil
		}
		if !isDir && e.Size > 0 {
			e.Offset = int64(childLBA) * BlockSize
		}
		if err := fn(e); err != nil {
			return err
		}
		if isDir {
			if childLength == 0 || childLength%BlockSize != 0 {
				return fmt.Errorf("iso9660: directory %q: an extent of %d bytes is not a whole number of blocks", e.Path, childLength)
			}
			if err := rd.walkDir(childLBA, childLength, e.Path, depth+1, fn); err != nil {
				return err
			}
		}
	}
	if pending != nil {
		return fmt.Errorf("iso9660: directory %q: the records of %q end without a final extent", dirPath, pending.Path)
	}
	return nil
}

// decodeIdentifier turns the identifier of a directory record back into a
// name: the version suffix of a file identifier is dropped, as is a trailing
// dot that only separated the name from an empty extension.
func decodeIdentifier(id []byte, isDir bool) string {
	name := string(id)
	if isDir {
		return name
	}
	if i := strings.LastIndexByte(name, ';'); i >= 0 {
		name = name[:i]
	}
	return strings.TrimSuffix(name, ".")
}

// decodeShortTime decodes the seven byte recording time of a directory record.
func decodeShortTime(b []byte) time.Time {
	if b[0] == 0 && b[1] == 0 && b[2] == 0 {
		return time.Time{}
	}
	offset := time.Duration(int8(b[6])) * 15 * time.Minute
	return time.Date(1900+int(b[0]), time.Month(b[1]), int(b[2]),
		int(b[3]), int(b[4]), int(b[5]), 0, time.UTC).Add(-offset)
}

// suspValue is one System Use entry, as read back.
type suspValue struct {
	signature string
	payload   []byte
}

// suspOf splits the system use area of a directory record into entries.
func suspOf(rec []byte) []suspValue {
	idLen := int(rec[32])
	pos := 33 + idLen
	if pos%2 != 0 {
		pos++
	}
	return suspIn(rec[min(pos, len(rec)):])
}

// suspIn splits a run of bytes into System Use entries. It stops at the first
// thing that is not one, which is how a padding byte at the end of a record
// ends the area.
func suspIn(area []byte) []suspValue {
	var out []suspValue
	for len(area) >= 4 {
		length := int(area[2])
		if length < 4 || length > len(area) {
			break
		}
		out = append(out, suspValue{signature: string(area[:2]), payload: area[4:length]})
		area = area[length:]
	}
	return out
}

// rockRidgeMeta is what the Rock Ridge entries of a record say.
type rockRidgeMeta struct {
	name         string
	target       string
	mode         uint32
	nlink        int
	uid, gid     uint32
	major, minor uint32
	hasPX, hasTF bool
	mtime        time.Time
	atime        time.Time
	ctime        time.Time
	// slContinuing records that the last symbolic link component was marked
	// as continued, so the next one appends to it.
	slContinuing bool
}

// applyTo overwrites what a record said with what Rock Ridge says.
func (m *rockRidgeMeta) applyTo(e *Entry) {
	if m.hasPX {
		e.Mode = modeFromPOSIX(m.mode)
		e.Nlink = m.nlink
		e.Uid, e.Gid = int(m.uid), int(m.gid)
	}
	if m.hasTF {
		if !m.mtime.IsZero() {
			e.Mtime = m.mtime
		}
		if !m.atime.IsZero() {
			e.Atime = m.atime
		}
		if !m.ctime.IsZero() {
			e.Ctime = m.ctime
		}
	}
	if m.target != "" {
		e.Target = m.target
	}
	e.Major, e.Minor = m.major, m.minor
}

// rockRidgeOf reads the Rock Ridge entries of a record, following the
// continuation area if the record points at one.
func (rd *Reader) rockRidgeOf(rec []byte) (*rockRidgeMeta, error) {
	m := &rockRidgeMeta{}
	entries := suspOf(rec)
	// A continuation area may itself point at another one; bound the chain.
	for depth := 0; depth < 8; depth++ {
		var next *ceRef
		for _, entry := range entries {
			switch entry.signature {
			case "PX":
				if len(entry.payload) < 32 {
					return nil, errors.New("iso9660: a PX entry is too short")
				}
				m.hasPX = true
				m.mode = binary.LittleEndian.Uint32(entry.payload[0:])
				m.nlink = int(binary.LittleEndian.Uint32(entry.payload[8:]))
				m.uid = binary.LittleEndian.Uint32(entry.payload[16:])
				m.gid = binary.LittleEndian.Uint32(entry.payload[24:])
			case "PN":
				if len(entry.payload) < 16 {
					return nil, errors.New("iso9660: a PN entry is too short")
				}
				m.major = binary.LittleEndian.Uint32(entry.payload[0:])
				m.minor = binary.LittleEndian.Uint32(entry.payload[8:])
			case "TF":
				if err := m.readTF(entry.payload); err != nil {
					return nil, err
				}
			case "NM":
				if len(entry.payload) < 1 {
					return nil, errors.New("iso9660: an NM entry is too short")
				}
				if entry.payload[0]&(nmCurrent|nmParent) == 0 {
					m.name += string(entry.payload[1:])
				}
			case "SL":
				if err := m.readSL(entry.payload); err != nil {
					return nil, err
				}
			case "CE":
				if len(entry.payload) < 24 {
					return nil, errors.New("iso9660: a CE entry is too short")
				}
				next = &ceRef{
					block:  binary.LittleEndian.Uint32(entry.payload[0:]),
					offset: binary.LittleEndian.Uint32(entry.payload[8:]),
					length: binary.LittleEndian.Uint32(entry.payload[16:]),
				}
			}
		}
		if next == nil {
			return m, nil
		}
		if next.length > 2*BlockSize {
			return nil, fmt.Errorf("iso9660: a continuation area of %d bytes is not one this package writes", next.length)
		}
		area := make([]byte, next.length)
		if err := readFull(rd.r, area, int64(next.block)*BlockSize+int64(next.offset)); err != nil {
			return nil, err
		}
		entries = suspIn(area)
	}
	return nil, errors.New("iso9660: the continuation areas of a record form a chain that is too long")
}

// readTF decodes a TF entry, which carries the timestamps that its flags
// select, in the order the flags are defined in.
func (m *rockRidgeMeta) readTF(payload []byte) error {
	if len(payload) < 1 {
		return errors.New("iso9660: a TF entry is too short")
	}
	flags := payload[0]
	if flags&0x80 != 0 {
		return errors.New("iso9660: a TF entry uses the long timestamp form, which this package does not write")
	}
	m.hasTF = true
	rest := payload[1:]
	for _, f := range []struct {
		bit  byte
		dest *time.Time
	}{
		{tfCreation, nil},
		{tfModify, &m.mtime},
		{tfAccess, &m.atime},
		{tfAttributes, &m.ctime},
	} {
		if flags&f.bit == 0 {
			continue
		}
		if len(rest) < 7 {
			return errors.New("iso9660: a TF entry is shorter than its flags say")
		}
		if f.dest != nil {
			*f.dest = decodeShortTime(rest[:7])
		}
		rest = rest[7:]
	}
	return nil
}

// readSL decodes an SL entry, appending its components to the target. A
// component whose record carries the continue flag is continued by the next
// record, with no separator in between.
func (m *rockRidgeMeta) readSL(payload []byte) error {
	if len(payload) < 1 {
		return errors.New("iso9660: an SL entry is too short")
	}
	rest := payload[1:]
	for len(rest) >= 2 {
		flags, length := rest[0], int(rest[1])
		if 2+length > len(rest) {
			return errors.New("iso9660: an SL component is longer than its entry")
		}
		text := string(rest[2 : 2+length])
		rest = rest[2+length:]
		switch {
		case flags&slCompRoot != 0:
			m.target += "/"
			m.slContinuing = false
			continue
		case flags&slCompCurrent != 0:
			text = "."
		case flags&slCompParent != 0:
			text = ".."
		}
		if !m.slContinuing && m.target != "" && !strings.HasSuffix(m.target, "/") {
			m.target += "/"
		}
		m.target += text
		m.slContinuing = flags&slCompContinue != 0
	}
	return nil
}

// modeFromPOSIX turns the mode of a PX entry into an [io/fs.FileMode].
func modeFromPOSIX(mode uint32) fs.FileMode {
	out := fs.FileMode(mode & 0o777)
	switch mode & 0o170000 {
	case 0o040000:
		out |= fs.ModeDir
	case 0o120000:
		out |= fs.ModeSymlink
	case 0o020000:
		out |= fs.ModeCharDevice | fs.ModeDevice
	case 0o060000:
		out |= fs.ModeDevice
	case 0o010000:
		out |= fs.ModeNamedPipe
	}
	if mode&0o4000 != 0 {
		out |= fs.ModeSetuid
	}
	if mode&0o2000 != 0 {
		out |= fs.ModeSetgid
	}
	if mode&0o1000 != 0 {
		out |= fs.ModeSticky
	}
	return out
}
