package ztoc

// This file contains a small, self-contained FlatBuffers encoder and decoder,
// sufficient for the ztoc schema. It is a faithful port of the relevant parts
// of github.com/google/flatbuffers/go (Apache-2.0), reimplemented here so this
// package needs no external FlatBuffers dependency. The builder performs the
// same vtable deduplication and alignment as the upstream library, so its
// output is byte-for-byte identical to soci-snapshotter's marshaled ztocs given
// an equivalent in-memory ztoc.

import (
	"encoding/binary"
	"errors"
)

const (
	sizeByte      = 1
	sizeUOffsetT  = 4 // relative offset to a table/string/vector
	sizeSOffsetT  = 4 // signed offset from a table to its vtable
	sizeVOffsetT  = 2 // offset within a vtable
	sizeInt8      = 1
	sizeInt32     = 4
	sizeInt64     = 8
	vtableMetaLen = 2 // number of leading uint16 metadata fields in a vtable
)

// fbBuilder builds a FlatBuffer back-to-front, matching google/flatbuffers/go.
type fbBuilder struct {
	bytes    []byte
	head     int // index of the next byte to write, decreasing toward 0
	minalign int

	vtable    []uint32 // current object's field offsets, indexed by slot
	objectEnd uint32
	vtables   []uint32 // offsets of previously written vtables (for dedup)
	nested    bool
}

func newFbBuilder(initialSize int) *fbBuilder {
	if initialSize <= 0 {
		initialSize = 1024
	}
	return &fbBuilder{
		bytes:    make([]byte, initialSize),
		head:     initialSize,
		minalign: 1,
		vtables:  make([]uint32, 0, 16),
	}
}

// offset returns the current write offset measured from the end of the buffer.
func (b *fbBuilder) offset() uint32 {
	return uint32(len(b.bytes) - b.head)
}

func (b *fbBuilder) growBuffer() {
	old := b.bytes
	newSize := len(old) * 2
	if newSize == 0 {
		newSize = 1
	}
	b.bytes = make([]byte, newSize)
	copy(b.bytes[newSize-len(old):], old)
	b.head += newSize - len(old)
}

// prep ensures that, after writing additionalBytes, a size-aligned write of
// `size` bytes will be correctly aligned, growing the buffer if necessary.
func (b *fbBuilder) prep(size, additionalBytes int) {
	if size > b.minalign {
		b.minalign = size
	}
	alignSize := (^(len(b.bytes) - b.head + additionalBytes) + 1) & (size - 1)
	for b.head < alignSize+size+additionalBytes {
		b.growBuffer()
	}
	for i := 0; i < alignSize; i++ {
		b.head--
		b.bytes[b.head] = 0
	}
}

func (b *fbBuilder) placeByte(x byte) {
	b.head--
	b.bytes[b.head] = x
}

func (b *fbBuilder) placeUint32(x uint32) {
	b.head -= 4
	binary.LittleEndian.PutUint32(b.bytes[b.head:], x)
}

func (b *fbBuilder) placeInt32(x int32) { b.placeUint32(uint32(x)) }

func (b *fbBuilder) placeInt64(x int64) {
	b.head -= 8
	binary.LittleEndian.PutUint64(b.bytes[b.head:], uint64(x))
}

func (b *fbBuilder) placeUint16(x uint16) {
	b.head -= 2
	binary.LittleEndian.PutUint16(b.bytes[b.head:], x)
}

func (b *fbBuilder) prependInt8(x int8)       { b.prep(sizeInt8, 0); b.placeByte(byte(x)) }
func (b *fbBuilder) prependInt32(x int32)     { b.prep(sizeInt32, 0); b.placeInt32(x) }
func (b *fbBuilder) prependUint32(x uint32)   { b.prep(sizeUOffsetT, 0); b.placeUint32(x) }
func (b *fbBuilder) prependInt64(x int64)     { b.prep(sizeInt64, 0); b.placeInt64(x) }
func (b *fbBuilder) prependVOffsetT(x uint16) { b.prep(sizeVOffsetT, 0); b.placeUint16(x) }

// prependUOffsetT writes a relative reference to a previously written object.
func (b *fbBuilder) prependUOffsetT(off uint32) {
	b.prep(sizeUOffsetT, 0)
	if off > b.offset() {
		panic("ztoc: flatbuffer offset points forward")
	}
	off2 := b.offset() - off + sizeUOffsetT
	b.placeUint32(off2)
}

// prependSOffsetT writes a signed offset placeholder (used for the vtable link).
func (b *fbBuilder) prependSOffsetT(off int32) {
	b.prep(sizeSOffsetT, 0)
	b.placeInt32(off)
}

// createString writes a null-terminated string and returns its offset.
func (b *fbBuilder) createString(s string) uint32 {
	b.nested = true
	b.prep(sizeUOffsetT, (len(s)+1)*sizeByte)
	b.placeByte(0)
	b.head -= len(s)
	copy(b.bytes[b.head:b.head+len(s)], s)
	return b.endVector(len(s))
}

// createByteVector writes a byte vector and returns its offset.
func (b *fbBuilder) createByteVector(v []byte) uint32 {
	b.nested = true
	b.prep(sizeUOffsetT, len(v)*sizeByte)
	b.head -= len(v)
	copy(b.bytes[b.head:b.head+len(v)], v)
	return b.endVector(len(v))
}

// startVector reserves alignment for a vector of numElems elements.
func (b *fbBuilder) startVector(elemSize, numElems, alignment int) uint32 {
	b.nested = true
	b.prep(sizeUOffsetT, elemSize*numElems)
	b.prep(alignment, elemSize*numElems)
	return b.offset()
}

// endVector writes the element count prefix and returns the vector's offset.
func (b *fbBuilder) endVector(numElems int) uint32 {
	b.placeUint32(uint32(numElems))
	b.nested = false
	return b.offset()
}

// startObject begins a table with numFields slots.
func (b *fbBuilder) startObject(numFields int) {
	b.nested = true
	if cap(b.vtable) < numFields {
		b.vtable = make([]uint32, numFields)
	} else {
		b.vtable = b.vtable[:numFields]
		for i := range b.vtable {
			b.vtable[i] = 0
		}
	}
	b.objectEnd = b.offset()
}

func (b *fbBuilder) slot(slotnum int) { b.vtable[slotnum] = b.offset() }

func (b *fbBuilder) prependInt64Slot(o int, x, d int64) {
	if x != d {
		b.prependInt64(x)
		b.slot(o)
	}
}

func (b *fbBuilder) prependInt32Slot(o int, x, d int32) {
	if x != d {
		b.prependInt32(x)
		b.slot(o)
	}
}

func (b *fbBuilder) prependInt8Slot(o int, x, d int8) {
	if x != d {
		b.prependInt8(x)
		b.slot(o)
	}
}

func (b *fbBuilder) prependUint32Slot(o int, x, d uint32) {
	if x != d {
		b.prependUint32(x)
		b.slot(o)
	}
}

func (b *fbBuilder) prependUOffsetTSlot(o int, x, d uint32) {
	if x != d {
		b.prependUOffsetT(x)
		b.slot(o)
	}
}

// endObject writes the object's vtable (deduplicating against prior vtables)
// and returns the object's offset.
func (b *fbBuilder) endObject() uint32 {
	return b.writeVtable()
}

func (b *fbBuilder) writeVtable() uint32 {
	// Reserve the slot for the soffset to the vtable.
	b.prependSOffsetT(0)
	objectOffset := b.offset()

	// Trim trailing empty slots.
	i := len(b.vtable) - 1
	for ; i >= 0 && b.vtable[i] == 0; i-- {
	}
	b.vtable = b.vtable[:i+1]

	// Search existing vtables for an identical one.
	var existing uint32
	for j := len(b.vtables) - 1; j >= 0; j-- {
		vt2Offset := b.vtables[j]
		vt2Start := len(b.bytes) - int(vt2Offset)
		vt2Len := int(binary.LittleEndian.Uint16(b.bytes[vt2Start:]))
		meta := vtableMetaLen * sizeVOffsetT
		vt2End := vt2Start + vt2Len
		vt2 := b.bytes[vt2Start+meta : vt2End]
		if b.vtableEqual(objectOffset, vt2) {
			existing = vt2Offset
			break
		}
	}

	if existing == 0 {
		// Write a new vtable, field slots first (in reverse).
		for k := len(b.vtable) - 1; k >= 0; k-- {
			var off uint32
			if b.vtable[k] != 0 {
				off = objectOffset - b.vtable[k]
			}
			b.prependVOffsetT(uint16(off))
		}
		b.prependVOffsetT(uint16(objectOffset - b.objectEnd)) // object length
		vBytes := (len(b.vtable) + vtableMetaLen) * sizeVOffsetT
		b.prependVOffsetT(uint16(vBytes)) // vtable length

		// Patch the object's soffset to point back to this vtable.
		objectStart := len(b.bytes) - int(objectOffset)
		binary.LittleEndian.PutUint32(b.bytes[objectStart:], uint32(int32(b.offset())-int32(objectOffset)))
		b.vtables = append(b.vtables, b.offset())
	} else {
		// Point the object at the existing vtable.
		objectStart := len(b.bytes) - int(objectOffset)
		b.head = objectStart
		binary.LittleEndian.PutUint32(b.bytes[b.head:], uint32(int32(existing)-int32(objectOffset)))
	}
	b.vtable = b.vtable[:0]
	return objectOffset
}

// vtableEqual reports whether the current builder vtable equals an on-buffer
// vtable's field-offset region.
func (b *fbBuilder) vtableEqual(objectOffset uint32, vt2 []byte) bool {
	if len(b.vtable)*sizeVOffsetT != len(vt2) {
		return false
	}
	for i := 0; i < len(b.vtable); i++ {
		x := binary.LittleEndian.Uint16(vt2[i*sizeVOffsetT:])
		// Skip 0 fields on both sides.
		if x == 0 && b.vtable[i] == 0 {
			continue
		}
		y := objectOffset - b.vtable[i]
		if uint32(x) != y {
			return false
		}
	}
	return true
}

// finish writes the root offset and marks the buffer complete.
func (b *fbBuilder) finish(rootTable uint32) {
	b.prep(b.minalign, sizeUOffsetT)
	b.prependUOffsetT(rootTable)
}

func (b *fbBuilder) finishedBytes() []byte {
	return b.bytes[b.head:]
}

// ---- reader ----

// fbTable is a view onto a table within a FlatBuffer.
type fbTable struct {
	buf []byte
	pos uint32
}

var errBadFlatbuffer = errors.New("ztoc: malformed ztoc flatbuffer")

// getRootTable resolves the root table of a finished FlatBuffer.
func getRootTable(buf []byte) (fbTable, error) {
	if len(buf) < sizeUOffsetT {
		return fbTable{}, errBadFlatbuffer
	}
	n := binary.LittleEndian.Uint32(buf)
	if int(n) > len(buf) {
		return fbTable{}, errBadFlatbuffer
	}
	return fbTable{buf: buf, pos: n}, nil
}

// offset returns the byte offset (relative to the table position) of the field
// stored in the given vtable slot, or 0 if the field is absent.
func (t fbTable) offset(vtableOffset uint16) uint32 {
	soff := int32(binary.LittleEndian.Uint32(t.buf[t.pos:]))
	vtable := uint32(int32(t.pos) - soff)
	vtLen := binary.LittleEndian.Uint16(t.buf[vtable:])
	if vtableOffset < vtLen {
		return uint32(binary.LittleEndian.Uint16(t.buf[vtable+uint32(vtableOffset):]))
	}
	return 0
}

// indirect follows a uoffset stored at the absolute offset off.
func (t fbTable) indirect(off uint32) uint32 {
	return off + binary.LittleEndian.Uint32(t.buf[off:])
}

func (t fbTable) getInt64(off uint32) int64   { return int64(binary.LittleEndian.Uint64(t.buf[off:])) }
func (t fbTable) getInt32(off uint32) int32   { return int32(binary.LittleEndian.Uint32(t.buf[off:])) }
func (t fbTable) getUint32(off uint32) uint32 { return binary.LittleEndian.Uint32(t.buf[off:]) }
func (t fbTable) getInt8(off uint32) int8     { return int8(t.buf[off]) }

// byteVector returns the bytes of a string/byte-vector whose header is at the
// absolute offset off (which holds a uoffset to the vector).
func (t fbTable) byteVector(off uint32) []byte {
	off += binary.LittleEndian.Uint32(t.buf[off:])
	length := binary.LittleEndian.Uint32(t.buf[off:])
	start := off + sizeUOffsetT
	return t.buf[start : start+length]
}

// vector returns the absolute offset of the first element of the vector field
// whose relative field offset is fieldOff.
func (t fbTable) vector(fieldOff uint32) uint32 {
	off := fieldOff + t.pos
	off += binary.LittleEndian.Uint32(t.buf[off:])
	return off + sizeUOffsetT
}

// vectorLen returns the element count of the vector field at relative fieldOff.
func (t fbTable) vectorLen(fieldOff uint32) int {
	off := fieldOff + t.pos
	off += binary.LittleEndian.Uint32(t.buf[off:])
	return int(binary.LittleEndian.Uint32(t.buf[off:]))
}

// vectorRange resolves a vector field to (start offset of the first element,
// element count), validating that the declared element count actually fits in
// the buffer. It panics (caught by Unmarshal's recover) on a corrupt length so
// that a malformed ztoc cannot drive a huge allocation or a runaway loop.
// elemSize is the size in bytes of each element.
func (t fbTable) vectorRange(fieldOff uint32, elemSize int) (start uint32, n int) {
	n = t.vectorLen(fieldOff)
	start = t.vector(fieldOff)
	if n < 0 || uint64(start)+uint64(n)*uint64(elemSize) > uint64(len(t.buf)) {
		panic(errBadFlatbuffer)
	}
	return start, n
}

// stringField returns the string value of a field at vtable slot, or "".
func (t fbTable) stringField(vtableOffset uint16) string {
	if o := t.offset(vtableOffset); o != 0 {
		return string(t.byteVector(o + t.pos))
	}
	return ""
}
