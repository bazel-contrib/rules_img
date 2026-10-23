package fat32

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"
)

const (
	attrReadOnly  = 0x01
	attrVolumeID  = 0x08
	attrDirectory = 0x10
	attrArchive   = 0x20

	endOfChain = 0x0FFFFFFF

	copyBufferSize = 1 << 20
)

var le = binary.LittleEndian

// truncater is implemented by files that can be extended without writing.
type truncater interface {
	Truncate(size int64) error
}

// Build lays out the filesystem and writes it to w, which must read as zeros
// wherever nothing was written (see the package documentation). Build may be
// called once. Content passed by path is read now, so the files must still
// be there.
func (b *Builder) Build(w io.WriterAt) (*Info, error) {
	if b.built {
		return nil, errors.New("fat32: image already built")
	}
	b.built = true

	l, err := b.plan()
	if err != nil {
		return nil, err
	}
	e := &emitter{
		b:    b,
		l:    l,
		w:    w,
		ss:   int(l.sectorSize),
		buf:  make([]byte, copyBufferSize),
		zero: make([]byte, max(int(l.sectorSize), int(l.clusterSize))),
	}
	if err := e.writeFiles(); err != nil {
		return nil, err
	}
	e.writeDirectories()
	e.writeFATs()
	e.writeBootArea()
	if e.err != nil {
		return nil, e.err
	}
	return e.finish()
}

type emitter struct {
	b    *Builder
	l    *layout
	w    io.WriterAt
	ss   int
	buf  []byte
	zero []byte

	// end is the end of the furthest byte written.
	end int64
	err error
}

// writeSparse writes p at off, leaving out the sectors that are all zero.
func (e *emitter) writeSparse(off int64, p []byte) {
	for i := 0; i < len(p) && e.err == nil; {
		end := min(i+e.ss, len(p))
		if bytes.Equal(p[i:end], e.zero[:end-i]) {
			i = end
			continue
		}
		j := end
		for j < len(p) {
			next := min(j+e.ss, len(p))
			if bytes.Equal(p[j:next], e.zero[:next-j]) {
				break
			}
			j = next
		}
		e.write(off+int64(i), p[i:j])
		i = j
	}
}

func (e *emitter) write(off int64, p []byte) {
	if e.err != nil || len(p) == 0 {
		return
	}
	if _, err := e.w.WriteAt(p, off); err != nil {
		e.err = fmt.Errorf("fat32: writing at offset %d: %w", off, err)
		return
	}
	e.end = max(e.end, off+int64(len(p)))
}

// clusterOffset is the byte offset of a data cluster.
func (e *emitter) clusterOffset(cluster uint32) int64 {
	l := e.l
	return int64((l.dataStart + uint64(cluster-2)*l.spc) * l.sectorSize)
}

// writeFiles streams the content of every regular file to its run of clusters.
func (e *emitter) writeFiles() error {
	for _, n := range e.l.nodes {
		if n.kind != kindReg || n.size == 0 {
			continue
		}
		if err := e.writeFile(n); err != nil {
			return err
		}
	}
	return e.err
}

func (e *emitter) writeFile(n *node) error {
	rc, err := n.src.open()
	if err != nil {
		return fmt.Errorf("fat32: %s: %w", n.planned.path, err)
	}
	defer rc.Close()

	var digest hash.Hash
	if e.b.opts.DigestFiles {
		digest = sha256.New()
	}
	off := e.clusterOffset(n.planned.first)
	for remaining := n.size; remaining > 0; {
		chunk := int(min(int64(len(e.buf)), remaining))
		if _, err := io.ReadFull(rc, e.buf[:chunk]); err != nil {
			return fmt.Errorf("fat32: %s: reading content: %w (declared size %d)", n.planned.path, unexpected(err), n.size)
		}
		if digest != nil {
			digest.Write(e.buf[:chunk])
		}
		e.writeSparse(off, e.buf[:chunk])
		if e.err != nil {
			return e.err
		}
		off += int64(chunk)
		remaining -= int64(chunk)
	}
	if digest != nil {
		n.planned.digest = digest.Sum(nil)
	}
	return nil
}

// fatDate and fatTime encode a timestamp the way directory entries store them.
// The range of FAT is 1980 to 2107; times outside are clamped.
func (e *emitter) fatTime(t time.Time) (date, tm uint16, tenth byte) {
	if t.IsZero() {
		t = e.b.opts.Time
	}
	t = t.UTC()
	if t.Year() < 1980 {
		t = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if t.Year() > 2107 {
		t = time.Date(2107, 12, 31, 23, 59, 58, 0, time.UTC)
	}
	date = uint16(t.Year()-1980)<<9 | uint16(t.Month())<<5 | uint16(t.Day())
	tm = uint16(t.Hour())<<11 | uint16(t.Minute())<<5 | uint16(t.Second()/2)
	tenth = byte(t.Second()%2*100 + t.Nanosecond()/10_000_000)
	return date, tm, tenth
}

// entry encodes a short directory entry.
func (e *emitter) entry(name [11]byte, attr byte, n *node, first uint32, size uint32) [32]byte {
	var d [32]byte
	copy(d[:11], name[:])
	d[11] = attr
	mdate, mtime, tenth := e.fatTime(n.mtime)
	adate, _, _ := e.fatTime(n.atime)
	d[13] = tenth
	le.PutUint16(d[14:], mtime) // creation: the modification time
	le.PutUint16(d[16:], mdate)
	le.PutUint16(d[18:], adate)
	le.PutUint16(d[20:], uint16(first>>16))
	le.PutUint16(d[22:], mtime)
	le.PutUint16(d[24:], mdate)
	le.PutUint16(d[26:], uint16(first))
	le.PutUint32(d[28:], size)
	return d
}

// writeDirectories writes the clusters of every directory.
func (e *emitter) writeDirectories() {
	l := e.l
	for _, n := range l.nodes {
		if n.kind != kindDir {
			continue
		}
		p := &n.planned
		data := make([]byte, uint64(p.clusters)*l.clusterSize)
		pos := 0
		put := func(d [32]byte) {
			copy(data[pos:], d[:])
			pos += 32
		}
		isRoot := n == e.rootNode()
		if isRoot && l.hasLabel {
			put(e.entry(l.label, attrVolumeID, n, 0, 0))
		}
		if !isRoot {
			put(e.entry(shortNameOf(".", ""), attrDirectory, n, p.first, 0))
			parent := p.parent.planned.first
			if p.parent == e.rootNode() {
				parent = 0 // the parent is the root: cluster 0, not its real number
			}
			put(e.entry(shortNameOf("..", ""), attrDirectory, n, parent, 0))
		}
		for _, ent := range p.dirents {
			if ent.long {
				for _, d := range longEntries(ent.name, shortChecksum(ent.short)) {
					put(d)
				}
			}
			child := ent.child
			if child.kind == kindDir {
				put(e.entry(ent.short, attrDirectory, child, child.planned.first, 0))
				continue
			}
			attr := byte(attrArchive)
			if child.mode&0o200 == 0 {
				attr |= attrReadOnly
			}
			put(e.entry(ent.short, attr, child, child.planned.first, uint32(child.size)))
		}
		e.writeSparse(e.clusterOffset(p.first), data)
	}
}

func (e *emitter) rootNode() *node { return e.l.nodes[0] }

// writeFATs writes both copies of the FAT. Every run of clusters is a chain
// that points to the next cluster and ends in the end-of-chain mark; the free
// remainder stays zero.
func (e *emitter) writeFATs() {
	l := e.l
	fat := make([]byte, (uint64(l.usedClusters)+2)*4)
	le.PutUint32(fat[0:], 0x0FFFFF00|0xF8) // media descriptor
	le.PutUint32(fat[4:], endOfChain)      // clean shutdown, no errors
	for _, n := range l.nodes {
		p := &n.planned
		for i := uint32(0); i < p.clusters; i++ {
			next := p.first + i + 1
			if i == p.clusters-1 {
				next = endOfChain
			}
			le.PutUint32(fat[(p.first+i)*4:], next)
		}
	}
	for i := uint64(0); i < numFATs; i++ {
		e.writeSparse(int64((l.reserved+i*l.fatSectors)*l.sectorSize), fat)
	}
}

// writeBootArea writes the boot sector, the FSInfo sector and their backups.
func (e *emitter) writeBootArea() {
	l := e.l
	ss := e.ss

	boot := make([]byte, ss)
	boot[0], boot[1], boot[2] = 0xEB, 0x58, 0x90
	copy(boot[3:], "MSWIN4.1")
	le.PutUint16(boot[11:], uint16(ss))
	boot[13] = byte(l.spc)
	le.PutUint16(boot[14:], uint16(l.reserved))
	boot[16] = numFATs
	boot[21] = 0xF8
	le.PutUint16(boot[24:], 63)
	le.PutUint16(boot[26:], 255)
	le.PutUint32(boot[32:], uint32(l.totalSectors))
	le.PutUint32(boot[36:], uint32(l.fatSectors))
	le.PutUint32(boot[44:], rootCluster)
	le.PutUint16(boot[48:], fsInfoSector)
	le.PutUint16(boot[50:], backupSector)
	boot[64] = 0x80
	boot[66] = 0x29
	le.PutUint32(boot[67:], l.volumeID)
	copy(boot[71:], l.label[:])
	copy(boot[82:], "FAT32   ")
	boot[510], boot[511] = 0x55, 0xAA

	info := make([]byte, ss)
	le.PutUint32(info[0:], 0x41615252)
	le.PutUint32(info[484:], 0x61417272)
	free := l.clusters - l.usedClusters
	le.PutUint32(info[488:], free)
	next := uint32(0xFFFFFFFF)
	if free > 0 {
		next = l.usedClusters + 2
	}
	le.PutUint32(info[492:], next)
	le.PutUint32(info[508:], 0xAA550000)

	extra := make([]byte, ss) // the third sector of the boot record only holds the signature
	extra[510], extra[511] = 0x55, 0xAA

	for i, sector := range [][]byte{boot, info, extra} {
		e.write(int64(i*ss), sector)
		e.write(int64((backupSector+i)*ss), sector)
	}
	// The backup FSInfo sector is where the backup boot sector says.
	// (The same FSInfo location, sector 1, is recorded in both boot sectors.)
}

// finish makes sure the target has the size of the filesystem.
func (e *emitter) finish() (*Info, error) {
	l := e.l
	size := int64(l.totalSectors * l.sectorSize)
	if t, ok := e.w.(truncater); ok {
		if err := t.Truncate(size); err != nil {
			return nil, fmt.Errorf("fat32: sizing the image: %w", err)
		}
	} else if e.end < size {
		e.write(size-1, []byte{0})
	}
	if e.err != nil {
		return nil, e.err
	}

	info := &Info{
		Size:         size,
		SectorSize:   e.ss,
		ClusterSize:  int(l.clusterSize),
		Clusters:     l.clusters,
		UsedClusters: l.usedClusters,
		VolumeID:     l.volumeID,
	}
	for _, n := range l.nodes {
		if n.kind == kindReg && n.size > 0 {
			info.Files = append(info.Files, FileLayout{
				Path:   n.planned.path,
				Offset: e.clusterOffset(n.planned.first),
				Size:   n.size,
				Digest: n.planned.digest,
			})
		}
	}
	return info, nil
}
