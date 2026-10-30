package cpio

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// Entry is a member of an archive, as read by [Reader].
type Entry struct {
	// Name is the slash-separated path of the member, without leading "./".
	Name string
	// Mode holds the permission bits and the file type.
	Mode fs.FileMode
	// UID and GID are the owner of the member.
	UID, GID uint32
	// Mtime is the modification time.
	Mtime time.Time
	// Ino and Nlink are the inode number and the link count of the member.
	Ino, Nlink uint32
	// Major and Minor are the device number of a device node.
	Major, Minor uint32
	// Target is the target of a symbolic link.
	Target string
	// Size is the size of the content of a regular file. For the members of a
	// set of hard links, it is the size of the content that the last member
	// carries.
	Size int64
	// Offset is the position of the content of a regular file in the archive.
	// It is the one of the member that carries the content.
	Offset int64

	id fileID
}

// fileID identifies a file in an archive: inode numbers are only unique per
// device.
type fileID struct {
	dev [2]uint32
	ino uint32
}

// Reader reads the archives written by [Builder]. It also reads archives
// from other writers as long as they use the newc format.
type Reader struct {
	r io.ReaderAt
}

// OpenReader returns a Reader for the archive in r. Bytes after the trailer
// are ignored.
func OpenReader(r io.ReaderAt) *Reader { return &Reader{r: r} }

// Content returns a reader for the content of a regular file.
func (rd *Reader) Content(e *Entry) io.Reader {
	return io.NewSectionReader(rd.r, e.Offset, e.Size)
}

// Walk calls fn for every member in archive order. A "." member is skipped.
func (rd *Reader) Walk(fn func(*Entry) error) error {
	var entries []Entry
	// carrier maps the inode of a file with several links to the entry that
	// carries the content.
	carrier := map[fileID]int{}
	for pos := int64(0); ; {
		var raw [headerSize]byte
		if err := rd.readFull(raw[:], pos); err != nil {
			return fmt.Errorf("cpio: reading header at %d: %w", pos, err)
		}
		if string(raw[:6]) != magic {
			return fmt.Errorf("cpio: bad magic %q at %d", raw[:6], pos)
		}
		var f [13]uint32
		for i := range f {
			v, err := strconv.ParseUint(string(raw[6+i*8:6+(i+1)*8]), 16, 32)
			if err != nil {
				return fmt.Errorf("cpio: bad header field %d at %d: %w", i, pos, err)
			}
			f[i] = uint32(v)
		}
		id := fileID{dev: [2]uint32{f[7], f[8]}, ino: f[0]}
		ino, mode, uid, gid, nlink, mtime, size := f[0], f[1], f[2], f[3], f[4], f[5], int64(f[6])
		nameSize := int64(f[11])
		if nameSize == 0 || nameSize > maxNameBytes+1 {
			return fmt.Errorf("cpio: bad name size %d at %d", nameSize, pos)
		}
		name := make([]byte, nameSize)
		if err := rd.readFull(name, pos+headerSize); err != nil {
			return fmt.Errorf("cpio: reading name at %d: %w", pos, err)
		}
		if name[nameSize-1] != 0 {
			return fmt.Errorf("cpio: name at %d is not terminated", pos)
		}
		dataStart := pos + headerSize + nameSize
		dataStart += pad4(dataStart)
		pos = dataStart + size + pad4(size)

		n := string(name[:nameSize-1])
		if n == trailerName {
			break
		}
		n = strings.TrimPrefix(n, "./")
		e := Entry{
			Name:  n,
			Mode:  fileMode(mode),
			UID:   uid,
			GID:   gid,
			Mtime: time.Unix(int64(mtime), 0),
			Ino:   ino,
			Nlink: nlink,
			id:    id,
			Major: f[9],
			Minor: f[10],
		}
		switch {
		case e.Mode&fs.ModeSymlink != 0:
			if size > maxNameBytes {
				return fmt.Errorf("cpio: %s: symbolic link target of %d bytes is longer than %d", n, size, maxNameBytes)
			}
			target := make([]byte, size)
			if err := rd.readFull(target, dataStart); err != nil {
				return fmt.Errorf("cpio: %s: reading symbolic link target: %w", n, err)
			}
			e.Target = string(bytes.TrimRight(target, "\x00"))
		case e.Mode.IsRegular():
			e.Size, e.Offset = size, dataStart
			if nlink > 1 {
				if size > 0 {
					carrier[id] = len(entries)
				}
			}
		}
		if n != "." {
			entries = append(entries, e)
		}
	}

	// The members of a set of hard links have the content of the one that
	// carries it.
	for i := range entries {
		e := &entries[i]
		if e.Mode.IsRegular() && e.Nlink > 1 {
			if c, ok := carrier[e.id]; ok {
				e.Size, e.Offset = entries[c].Size, entries[c].Offset
			}
		}
	}
	for i := range entries {
		if err := fn(&entries[i]); err != nil {
			return err
		}
	}
	return nil
}

func (rd *Reader) readFull(p []byte, off int64) error {
	n, err := rd.r.ReadAt(p, off)
	if n == len(p) {
		return nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func fileMode(mode uint32) fs.FileMode {
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
	switch mode & modeTypeMask {
	case modeDir:
		m |= fs.ModeDir
	case modeSymlink:
		m |= fs.ModeSymlink
	case modeChar:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case modeBlock:
		m |= fs.ModeDevice
	case modeFifo:
		m |= fs.ModeNamedPipe
	case 0o140000:
		m |= fs.ModeSocket
	}
	return m
}
