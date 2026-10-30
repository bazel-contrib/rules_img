package cpio

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
)

// planned is what Build works out for a node.
type planned struct {
	ino   uint32
	nlink uint32
	// last is the index of the last member (in archive order) that references
	// the node. That member carries the content of a regular file.
	last int
	path string
}

// member is one entry of the archive.
type member struct {
	path string
	n    *node
}

// Build writes the archive to w. The content of the files is read now. A
// Builder can only build once.
func (b *Builder) Build(w io.Writer) (*Info, error) {
	if b.built {
		return nil, errors.New("cpio: archive already built")
	}
	b.built = true

	if err := b.deduplicate(); err != nil {
		return nil, err
	}
	members := b.plan()
	if err := b.spoolUnorderedTars(members); err != nil {
		return nil, err
	}

	cw := &countWriter{w: w}
	var out io.Writer = cw
	var archiveDigest hash.Hash
	if b.opts.DigestArchive {
		archiveDigest = sha256.New()
		out = io.MultiWriter(cw, archiveDigest)
	}
	bw := bufio.NewWriterSize(out, 1<<20)
	e := &emitter{b: b, w: bw, cw: cw, buf: make([]byte, 256<<10)}

	for i, m := range members {
		if err := e.writeMember(i, m); err != nil {
			return nil, err
		}
	}
	if err := e.writeRecord(trailerName, &node{}, 0, 0, 1, 0); err != nil {
		return nil, err
	}
	if err := bw.Flush(); err != nil {
		return nil, err
	}

	info := &Info{Size: cw.n, Entries: len(members), Files: e.files}
	if archiveDigest != nil {
		info.Digest = archiveDigest.Sum(nil)
	}
	return info, nil
}

// plan walks the tree depth-first in name order and assigns the inode numbers
// and link counts.
func (b *Builder) plan() []member {
	var members []member
	var next uint32
	var walk func(dir *node, prefix string)
	walk = func(dir *node, prefix string) {
		names := make([]string, 0, len(dir.children))
		for name := range dir.children {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			n := dir.children[name]
			p := prefix + name
			if n.planned.ino == 0 {
				next++
				n.planned.ino = next
			}
			if n.kind == kindDir {
				n.planned.nlink = 2
				for _, child := range n.children {
					if child.kind == kindDir {
						n.planned.nlink++
					}
				}
			} else {
				n.planned.nlink++
			}
			n.planned.last = len(members)
			n.planned.path = p
			members = append(members, member{path: p, n: n})
			if n.kind == kindDir {
				walk(n, p+"/")
			}
		}
	}
	walk(b.root, "")
	return members
}

type emitter struct {
	b     *Builder
	w     *bufio.Writer
	cw    *countWriter
	buf   []byte
	files []FileLayout
}

// pos is the position in the archive of the next byte written.
func (e *emitter) pos() int64 { return e.cw.n + int64(e.w.Buffered()) }

func (e *emitter) writeMember(index int, m member) error {
	n := m.n
	// The last member of a set of hard links carries the content.
	carries := n.kind == kindReg && n.planned.last == index
	var size int64
	switch n.kind {
	case kindReg:
		if carries {
			size = n.size
		}
	case kindSymlink:
		size = int64(len(n.target))
	}
	if err := e.writeRecord(m.path, n, typeBits(n.kind)|n.mode, n.planned.ino, n.planned.nlink, size); err != nil {
		return err
	}
	switch {
	case n.kind == kindSymlink:
		if _, err := e.w.WriteString(n.target); err != nil {
			return err
		}
		return e.pad(size)
	case carries && n.size > 0:
		if err := e.writeFile(m, n); err != nil {
			return err
		}
		return e.pad(size)
	}
	return nil
}

func (e *emitter) writeFile(m member, n *node) error {
	rc, err := n.src.open()
	if err != nil {
		return fmt.Errorf("cpio: %s: %w", m.path, err)
	}
	defer rc.Close()

	// A digest that is already known is not computed again.
	var digest hash.Hash
	if e.b.opts.DigestFiles && n.digest == nil {
		if d, ok := e.b.knownPathDigest(n); ok {
			n.digest = d
		} else {
			digest = sha256.New()
		}
	}
	offset := e.pos()
	for remaining := n.size; remaining > 0; {
		chunk := int(min(int64(len(e.buf)), remaining))
		if _, err := io.ReadFull(rc, e.buf[:chunk]); err != nil {
			return fmt.Errorf("cpio: %s: reading content: %w (declared size %d)", m.path, unexpected(err), n.size)
		}
		if digest != nil {
			digest.Write(e.buf[:chunk])
		}
		if _, err := e.w.Write(e.buf[:chunk]); err != nil {
			return err
		}
		remaining -= int64(chunk)
	}
	if digest != nil {
		n.digest = digest.Sum(nil)
		e.b.rememberPathDigest(n)
	}
	if e.b.opts.DigestFiles {
		e.files = append(e.files, FileLayout{Path: m.path, Offset: offset, Size: n.size, Digest: n.digest})
	} else {
		e.files = append(e.files, FileLayout{Path: m.path, Offset: offset, Size: n.size})
	}
	return nil
}

// pad pads the data of size bytes to a multiple of four bytes.
func (e *emitter) pad(size int64) error {
	_, err := e.w.Write(make([]byte, pad4(size)))
	return err
}

func (e *emitter) writeRecord(name string, n *node, mode, ino, nlink uint32, size int64) error {
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
	put(0, ino)
	put(1, mode)
	put(2, n.uid)
	put(3, n.gid)
	put(4, nlink)
	put(5, uint32(n.mtime))
	put(6, uint32(size))
	put(9, n.major)
	put(10, n.minor)
	put(11, uint32(len(name)+1))
	if _, err := e.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := e.w.WriteString(name); err != nil {
		return err
	}
	// The terminating NUL, then padding to a multiple of four bytes for the
	// header and the name together.
	total := int64(headerSize + len(name) + 1)
	_, err := e.w.Write(make([]byte, 1+pad4(total)))
	return err
}

func typeBits(k kind) uint32 {
	switch k {
	case kindDir:
		return modeDir
	case kindSymlink:
		return modeSymlink
	case kindChar:
		return modeChar
	case kindBlock:
		return modeBlock
	case kindFifo:
		return modeFifo
	}
	return modeReg
}
