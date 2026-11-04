package iso9660

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"time"
)

// File flags of a directory record.
const (
	flagDirectory = 1 << 1
	// flagMultiExtent marks a record whose file continues in the next record.
	flagMultiExtent = 1 << 7
)

const copyBufferSize = 1 << 20

// truncater is implemented by files that can be extended without writing.
type truncater interface {
	Truncate(size int64) error
}

// Build lays out the image and writes it to w, which must read as zeros
// wherever nothing was written (see the package documentation). Build may be
// called once. Content passed by path is read now, so the files must still be
// there.
func (b *Builder) Build(w io.WriterAt) (*Info, error) {
	if b.built {
		return nil, errors.New("iso9660: image already built")
	}
	b.built = true
	defer b.tars.close()

	var imageReader io.ReaderAt
	if b.opts.DigestImage {
		var ok bool
		if imageReader, ok = w.(io.ReaderAt); !ok {
			return nil, errors.New("iso9660: DigestImage needs a target that can be read back")
		}
	}

	l, err := b.plan()
	if err != nil {
		return nil, err
	}
	e := &emitter{
		b:       b,
		l:       l,
		w:       w,
		buf:     make([]byte, copyBufferSize),
		zero:    make([]byte, BlockSize),
		parents: map[*node]*node{},
	}
	for _, dir := range l.primary.dirs {
		if dir.parent != dir {
			e.parents[dir.node] = dir.parent.node
		}
	}

	// Everything but the data of the regular files is metadata whose content
	// does not depend on that data, so writing it first lets the file data be
	// written (and hashed) front to back afterwards.
	for _, h := range l.hierarchies() {
		e.writePathTable(h, false)
		e.writePathTable(h, true)
		e.writeDirectories(h)
	}
	e.writeContinuations()
	e.writeVolumeDescriptors()
	if len(l.boot) > 0 {
		e.writeBootRecord()
		e.writeBootCatalog()
	}
	if b.opts.UDF {
		if err := e.writeUDF(); err != nil {
			return nil, err
		}
	}
	if e.err != nil {
		return nil, e.err
	}

	size := int64(l.totalBlocks) * BlockSize
	// The target has to have its final size before anything is read back: the
	// trailing structures (the UDF anchor, the padding) are already written,
	// and the digest covers the whole volume.
	if err := e.resize(size); err != nil {
		return nil, err
	}
	if imageReader != nil {
		e.image = sha256.New()
		if err := e.hashRange(imageReader, 0, e.firstFileOffset()); err != nil {
			return nil, err
		}
	}
	if err := e.writeFiles(); err != nil {
		return nil, err
	}
	if e.image != nil {
		if err := e.hashRange(imageReader, e.imagePos, size); err != nil {
			return nil, err
		}
	}
	return e.finish(size)
}

type emitter struct {
	b *Builder
	l *layout
	w io.WriterAt

	buf  []byte
	zero []byte

	// end is the end of the furthest byte written.
	end int64
	err error

	// image hashes the image front to back if the digest is requested, and
	// imagePos is the offset up to which it has hashed.
	image    hash.Hash
	imagePos int64

	// parents maps a directory to the directory it is in, which UDF records in
	// the parent entry of a directory.
	parents map[*node]*node
}

// firstFileOffset is where the data of the regular files begins, or the end of
// the image if it holds no file with content.
func (e *emitter) firstFileOffset() int64 {
	if len(e.l.files) == 0 {
		return int64(e.l.totalBlocks) * BlockSize
	}
	return int64(e.l.files[0].planned.lba) * BlockSize
}

// hashRange hashes the bytes of the image between two offsets by reading them
// back. Unwritten ranges read as zeros, even beyond the end of the target.
func (e *emitter) hashRange(r io.ReaderAt, from, to int64) error {
	for pos := from; pos < to; {
		chunk := e.buf[:min(int64(len(e.buf)), to-pos)]
		n, err := r.ReadAt(chunk, pos)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("iso9660: reading back the image at %d: %w", pos, err)
		}
		clear(chunk[n:])
		e.image.Write(chunk)
		pos += int64(len(chunk))
	}
	e.imagePos = max(e.imagePos, to)
	return nil
}

// hashZeros hashes the zero bytes up to offset to, which is how the padding
// between the extents of two files is accounted for.
func (e *emitter) hashZeros(to int64) {
	for e.imagePos < to {
		n := min(int64(len(e.zero)), to-e.imagePos)
		e.image.Write(e.zero[:n])
		e.imagePos += n
	}
}

// writeSparse writes p at off, leaving out the blocks that are all zero.
func (e *emitter) writeSparse(off int64, p []byte) {
	for i := 0; i < len(p) && e.err == nil; {
		end := min(i+BlockSize, len(p))
		if bytes.Equal(p[i:end], e.zero[:end-i]) {
			i = end
			continue
		}
		j := end
		for j < len(p) {
			next := min(j+BlockSize, len(p))
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
		e.err = fmt.Errorf("iso9660: writing at offset %d: %w", off, err)
		return
	}
	e.end = max(e.end, off+int64(len(p)))
}

// writeBlocks writes data starting at a logical block.
func (e *emitter) writeBlocks(lba uint32, data []byte) {
	e.writeSparse(int64(lba)*BlockSize, data)
}

// writePathTable writes one of the two copies of a hierarchy's path table: the
// type L table holds its numbers little endian, the type M table big endian.
func (e *emitter) writePathTable(h *hierarchy, bigEndian bool) {
	data := make([]byte, uint64(blocksFor(int64(h.pathTableSize)))*BlockSize)
	pos := 0
	for _, dir := range h.dirs {
		id := pathTableIdentifier(dir)
		data[pos] = byte(len(id))
		data[pos+1] = 0 // no extended attribute record
		if bigEndian {
			putU32BE(data[pos+2:], dir.lba)
			putU16BE(data[pos+6:], dir.parent.number)
		} else {
			putU32LE(data[pos+2:], dir.lba)
			putU16LE(data[pos+6:], dir.parent.number)
		}
		copy(data[pos+8:], id)
		pos += 8 + len(id)
		if len(id)%2 != 0 {
			pos++ // the padding field that keeps records on even boundaries
		}
	}
	lba := h.lPathTable
	if bigEndian {
		lba = h.mPathTable
	}
	e.writeBlocks(lba, data)
}

// writeDirectories writes the extent of every directory of a hierarchy.
func (e *emitter) writeDirectories(h *hierarchy) {
	for _, dir := range h.dirs {
		data := make([]byte, dir.length)
		pos := 0
		for _, rec := range dir.records {
			// A record never crosses a logical block boundary; a block that
			// cannot hold the next one is left zero filled.
			if pos%BlockSize+rec.size > BlockSize {
				pos += BlockSize - pos%BlockSize
			}
			e.putRecord(data[pos:pos+rec.size], rec)
			pos += rec.size
		}
		e.writeBlocks(dir.lba, data)
	}
}

// putRecord writes one directory record.
func (e *emitter) putRecord(b []byte, rec *record) {
	lba, length := rec.location()
	b[0] = byte(len(b))
	b[1] = 0 // no extended attribute record
	putBoth32(b[2:], lba)
	putBoth32(b[10:], length)
	e.putShortTime(b[18:25], rec.node.mtime)
	if rec.isDir {
		b[25] |= flagDirectory
	}
	if !rec.lastExtent {
		b[25] |= flagMultiExtent
	}
	b[26] = 0 // file unit size: the file is not interleaved
	b[27] = 0 // interleave gap size
	putBoth16(b[28:], 1)
	b[32] = byte(len(rec.id))
	copy(b[33:], rec.id)
	pos := 33 + len(rec.id)
	if pos%2 != 0 {
		pos++ // the padding field
	}
	for _, entry := range rec.susp {
		copy(b[pos:], entry)
		pos += len(entry)
	}
	if len(rec.overflow) > 0 {
		copy(b[pos:], suspCE(rec.ce.block, rec.ce.offset, rec.ce.length))
	}
}

// putShortTime writes the seven byte recording time of a directory record,
// falling back to the default time of the image.
func (e *emitter) putShortTime(b []byte, t time.Time) {
	if t.IsZero() {
		t = e.b.opts.Time
	}
	copy(b, appendShortTime(nil, t))
}

// writeContinuations writes the Rock Ridge continuation areas, which hold the
// system use entries that did not fit in their directory record.
func (e *emitter) writeContinuations() {
	if e.l.ceBlocks == 0 {
		return
	}
	data := make([]byte, uint64(e.l.ceBlocks)*BlockSize)
	for _, dir := range e.l.primary.dirs {
		for _, rec := range dir.records {
			if len(rec.overflow) == 0 {
				continue
			}
			pos := int64(rec.ce.block-e.l.ceStart)*BlockSize + int64(rec.ce.offset)
			for _, entry := range rec.overflow {
				copy(data[pos:], entry)
				pos += int64(len(entry))
			}
		}
	}
	e.writeBlocks(e.l.ceStart, data)
}

// writeVolumeDescriptors writes the primary volume descriptor, the
// supplementary one that describes the Joliet hierarchy, and the terminator
// that ends the set.
func (e *emitter) writeVolumeDescriptors() {
	l := e.l
	e.writeBlocks(l.pvdLBA, e.volumeDescriptor(1, l.primary))
	if l.joliet != nil {
		e.writeBlocks(l.svdLBA, e.volumeDescriptor(2, l.joliet))
	}

	term := make([]byte, BlockSize)
	term[0] = 255
	copy(term[1:], "CD001")
	term[6] = 1
	e.writeBlocks(l.vdstLBA, term)
}

// volumeDescriptor builds the primary volume descriptor (type 1) or the
// supplementary one that describes the Joliet hierarchy (type 2). The two have
// the same layout; the Joliet one records an escape sequence selecting UCS-2
// and writes its identifiers in that encoding.
func (e *emitter) volumeDescriptor(kind byte, h *hierarchy) []byte {
	opts := &e.b.opts
	b := make([]byte, BlockSize)
	b[0] = kind
	copy(b[1:], "CD001")
	b[6] = 1
	if h.joliet {
		// The escape sequence that selects UCS-2 level 3, which is what Joliet
		// names are encoded in.
		copy(b[88:120], "%/E")
		putPaddedUCS2(b[8:40], opts.SystemID)
		putPaddedUCS2(b[40:72], opts.VolumeID)
	} else {
		putPadded(b[8:40], opts.SystemID, ' ')
		putPadded(b[40:72], opts.VolumeID, ' ')
	}
	putBoth32(b[80:], e.l.totalBlocks)
	putBoth16(b[120:], 1) // volume set size
	putBoth16(b[124:], 1) // volume sequence number
	putBoth16(b[128:], BlockSize)
	putBoth32(b[132:], h.pathTableSize)
	putU32LE(b[140:], h.lPathTable)
	putU32BE(b[148:], h.mPathTable)
	e.putRootRecord(b[156:190], h)

	fields := []struct {
		field []byte
		value string
	}{
		{b[190:318], opts.VolumeSetID},
		{b[318:446], opts.PublisherID},
		{b[446:574], opts.DataPreparerID},
		{b[574:702], opts.ApplicationID},
		{b[702:739], ""}, // copyright file identifier
		{b[739:776], ""}, // abstract file identifier
		{b[776:813], ""}, // bibliographic file identifier
	}
	for _, f := range fields {
		if h.joliet {
			putPaddedUCS2(f.field, f.value)
		} else {
			putPadded(f.field, f.value, ' ')
		}
	}
	putLongTime(b[813:830], e.l.time)
	putLongTime(b[830:847], e.l.time)
	putUnspecifiedTime(b[847:864]) // the volume does not expire
	putLongTime(b[864:881], e.l.time)
	b[881] = 1 // file structure version
	return b
}

// putRootRecord writes the 34 byte directory record for the root directory
// that a volume descriptor carries.
func (e *emitter) putRootRecord(b []byte, h *hierarchy) {
	root := h.root()
	b[0] = 34
	putBoth32(b[2:], root.lba)
	putBoth32(b[10:], root.length)
	e.putShortTime(b[18:25], root.node.mtime)
	b[25] = flagDirectory
	putBoth16(b[28:], 1)
	b[32] = 1
	b[33] = 0 // the identifier of the root directory
}

// putUnspecifiedTime writes the digits ISO 9660 uses for a date and time that
// is not specified.
func putUnspecifiedTime(b []byte) {
	putPadded(b[:16], "", '0')
	b[16] = 0
}

// writeFiles streams the content of every regular file to its extent.
func (e *emitter) writeFiles() error {
	for _, n := range e.l.files {
		if err := e.writeFile(n); err != nil {
			return err
		}
	}
	return e.err
}

func (e *emitter) writeFile(n *node) error {
	// A boot image that asked for a boot information table needs it patched
	// into its first block, which reads the content once to checksum it.
	table, err := e.bootTableFor(n)
	if err != nil {
		return err
	}

	rc, err := n.src.open()
	if err != nil {
		return fmt.Errorf("iso9660: %s: %w", n.planned.path, err)
	}
	defer rc.Close()

	// A digest that is already known is not computed again. A patched boot
	// image gets none at all: its stored content is not the file that went in,
	// so it must not become a content-addressed reference.
	var digest hash.Hash
	if e.b.opts.DigestFiles && !n.planned.patched && n.digest == nil {
		if d, ok := e.b.knownPathDigest(n); ok {
			n.digest = d
		} else {
			digest = sha256.New()
		}
	}

	off := int64(n.planned.lba) * BlockSize
	if e.image != nil {
		e.hashZeros(off) // the padding after the previous file's last block
		if e.imagePos != off {
			return fmt.Errorf("iso9660: %s: extent at %d, but the image digest is at %d", n.planned.path, off, e.imagePos)
		}
	}
	for written, remaining := int64(0), n.size; remaining > 0; {
		chunk := int(min(int64(len(e.buf)), remaining))
		if _, err := io.ReadFull(rc, e.buf[:chunk]); err != nil {
			return fmt.Errorf("iso9660: %s: reading content: %w (declared size %d)", n.planned.path, unexpected(err), n.size)
		}
		if digest != nil {
			digest.Write(e.buf[:chunk])
		}
		if table != nil {
			patchBootTable(e.buf[:chunk], written, table)
		}
		if e.image != nil {
			e.image.Write(e.buf[:chunk])
			e.imagePos += int64(chunk)
		}
		e.writeSparse(off, e.buf[:chunk])
		if e.err != nil {
			return e.err
		}
		off += int64(chunk)
		written += int64(chunk)
		remaining -= int64(chunk)
	}
	if digest != nil {
		n.digest = digest.Sum(nil)
		e.b.rememberPathDigest(n)
	}
	if e.b.opts.DigestFiles && !n.planned.patched {
		n.planned.digest = n.digest
	}
	return nil
}

// patchBootTable overwrites the part of buf that falls inside the boot
// information table, which occupies bytes 8 to 64 of a boot image. start is
// the offset buf has in the file.
func patchBootTable(buf []byte, start int64, table []byte) {
	const from = 8
	to := int64(from + len(table))
	lo, hi := max(start, from), min(start+int64(len(buf)), to)
	for i := lo; i < hi; i++ {
		buf[i-start] = table[i-from]
	}
}

// resize makes sure the target has the size of the image.
func (e *emitter) resize(size int64) error {
	if t, ok := e.w.(truncater); ok {
		if err := t.Truncate(size); err != nil {
			return fmt.Errorf("iso9660: sizing the image: %w", err)
		}
		return nil
	}
	if e.end < size {
		e.write(size-1, []byte{0})
	}
	return e.err
}

func (e *emitter) finish(size int64) (*Info, error) {
	if e.err != nil {
		return nil, e.err
	}
	l := e.l
	info := &Info{
		Size:        size,
		Blocks:      l.totalBlocks,
		RockRidge:   e.b.opts.RockRidge,
		Joliet:      e.b.opts.Joliet,
		UDF:         e.b.opts.UDF,
		BootCatalog: l.bootCatalogLBA,
	}
	if e.image != nil {
		info.Digest = e.image.Sum(nil)
	}
	for _, n := range l.files {
		info.Files = append(info.Files, FileLayout{
			Path:    n.planned.path,
			Offset:  int64(n.planned.lba) * BlockSize,
			Size:    n.size,
			Digest:  n.planned.digest,
			Patched: n.planned.patched,
		})
	}
	return info, nil
}
