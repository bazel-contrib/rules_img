package ztoc

import (
	"fmt"
	"sort"
	"time"

	"github.com/opencontainers/go-digest"
)

// This file marshals/unmarshals a Ztoc to/from its FlatBuffer form. The field
// order, slot numbers, and construction sequence deliberately mirror
// soci-snapshotter's ztoc_marshaler.go so that, given an equivalent Ztoc, the
// bytes produced here are identical to soci's.

// FlatBuffer vtable slot indices for each table (see the ztoc.fbs schema).
const (
	// Ztoc
	fZtocVersion         = 0
	fZtocBuildTool       = 1
	fZtocCompressedSize  = 2
	fZtocUncompSize      = 3
	fZtocTOC             = 4
	fZtocCompressionInfo = 5
	ztocNumFields        = 6

	// TOC
	fTOCMetadata = 0
	tocNumFields = 1

	// FileMetadata
	fFMName       = 0
	fFMType       = 1
	fFMUncompOff  = 2
	fFMUncompSize = 3
	fFMLinkname   = 4
	fFMMode       = 5
	fFMUID        = 6
	fFMGID        = 7
	fFMUname      = 8
	fFMGname      = 9
	fFMModTime    = 10
	fFMDevmajor   = 11
	fFMDevminor   = 12
	fFMXattrs     = 13
	fmNumFields   = 14

	// CompressionInfo
	fCIAlgorithm   = 0
	fCIMaxSpanID   = 1
	fCISpanDigests = 2
	fCICheckpoints = 3
	ciNumFields    = 4

	// Xattr
	fXattrKey      = 0
	fXattrValue    = 1
	xattrNumFields = 2
)

// CompressionAlgorithm enum values (ztoc.fbs).
const (
	algoGzip         int8 = 1
	algoUncompressed int8 = 2
)

// vo converts a table slot index to its vtable byte offset.
func vo(slot int) uint16 { return uint16(4 + slot*2) }

// Marshal serializes a Ztoc to its FlatBuffer representation.
func Marshal(z *Ztoc) ([]byte, error) {
	b := newFbBuilder(1024)

	version := b.createString(string(z.Version))
	buildTool := b.createString(z.BuildToolIdentifier)

	toc := marshalTOC(b, &z.TOC)

	checkpointsVec := b.createByteVector(z.Checkpoints)

	spanOffs := make([]uint32, len(z.SpanDigests))
	for i, d := range z.SpanDigests {
		spanOffs[i] = b.createString(d.String())
	}
	b.startVector(sizeUOffsetT, len(spanOffs), sizeUOffsetT)
	for i := len(spanOffs) - 1; i >= 0; i-- {
		b.prependUOffsetT(spanOffs[i])
	}
	spanDigests := b.endVector(len(spanOffs))

	b.startObject(ciNumFields)
	b.prependInt32Slot(fCIMaxSpanID, int32(z.MaxSpanID), 0)
	b.prependUOffsetTSlot(fCISpanDigests, spanDigests, 0)
	b.prependUOffsetTSlot(fCICheckpoints, checkpointsVec, 0)
	if z.CompressionAlgorithm != "" {
		algo, err := compressionAlgorithmToEnum(z.CompressionAlgorithm)
		if err != nil {
			return nil, err
		}
		b.prependInt8Slot(fCIAlgorithm, algo, algoGzip)
	}
	compressionInfo := b.endObject()

	b.startObject(ztocNumFields)
	b.prependUOffsetTSlot(fZtocVersion, version, 0)
	b.prependUOffsetTSlot(fZtocBuildTool, buildTool, 0)
	b.prependUOffsetTSlot(fZtocTOC, toc, 0)
	b.prependInt64Slot(fZtocCompressedSize, int64(z.CompressedArchiveSize), 0)
	b.prependInt64Slot(fZtocUncompSize, int64(z.UncompressedArchiveSize), 0)
	b.prependUOffsetTSlot(fZtocCompressionInfo, compressionInfo, 0)
	root := b.endObject()

	b.finish(root)
	return b.finishedBytes(), nil
}

func marshalTOC(b *fbBuilder, toc *TOC) uint32 {
	offs := make([]uint32, len(toc.FileMetadata))
	for i := len(toc.FileMetadata) - 1; i >= 0; i-- {
		offs[i] = marshalFileMetadata(b, &toc.FileMetadata[i])
	}
	b.startVector(sizeUOffsetT, len(offs), sizeUOffsetT)
	for i := len(offs) - 1; i >= 0; i-- {
		b.prependUOffsetT(offs[i])
	}
	metadata := b.endVector(len(offs))

	b.startObject(tocNumFields)
	b.prependUOffsetTSlot(fTOCMetadata, metadata, 0)
	return b.endObject()
}

func marshalFileMetadata(b *fbBuilder, me *FileMetadata) uint32 {
	name := b.createString(me.Name)
	typ := b.createString(me.Type)
	linkname := b.createString(me.Linkname)
	uname := b.createString(me.Uname)
	gname := b.createString(me.Gname)
	modTimeBin, _ := me.ModTime.MarshalText()
	modTime := b.createString(string(modTimeBin))
	xattrs := marshalXattrs(b, me.PAXHeaders)

	b.startObject(fmNumFields)
	b.prependUOffsetTSlot(fFMName, name, 0)
	b.prependUOffsetTSlot(fFMType, typ, 0)
	b.prependInt64Slot(fFMUncompOff, int64(me.UncompressedOffset), 0)
	b.prependInt64Slot(fFMUncompSize, int64(me.UncompressedSize), 0)
	b.prependUOffsetTSlot(fFMLinkname, linkname, 0)
	b.prependInt64Slot(fFMMode, me.Mode, 0)
	b.prependUint32Slot(fFMUID, uint32(me.UID), 0)
	b.prependUint32Slot(fFMGID, uint32(me.GID), 0)
	b.prependUOffsetTSlot(fFMUname, uname, 0)
	b.prependUOffsetTSlot(fFMGname, gname, 0)
	b.prependUOffsetTSlot(fFMModTime, modTime, 0)
	b.prependInt64Slot(fFMDevmajor, me.Devmajor, 0)
	b.prependInt64Slot(fFMDevminor, me.Devminor, 0)
	b.prependUOffsetTSlot(fFMXattrs, xattrs, 0)
	return b.endObject()
}

func marshalXattrs(b *fbBuilder, pax map[string]string) uint32 {
	keys := make([]string, 0, len(pax))
	for k := range pax {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	offs := make([]uint32, 0, len(keys))
	for _, k := range keys {
		keyOff := b.createString(k)
		valOff := b.createString(pax[k])
		b.startObject(xattrNumFields)
		b.prependUOffsetTSlot(fXattrKey, keyOff, 0)
		b.prependUOffsetTSlot(fXattrValue, valOff, 0)
		offs = append(offs, b.endObject())
	}
	b.startVector(sizeUOffsetT, len(offs), sizeUOffsetT)
	for j := len(offs) - 1; j >= 0; j-- {
		b.prependUOffsetT(offs[j])
	}
	return b.endVector(len(offs))
}

func compressionAlgorithmToEnum(algo string) (int8, error) {
	switch algo {
	case CompressionGzip:
		return algoGzip, nil
	case CompressionUncompressed:
		return algoUncompressed, nil
	default:
		return 0, fmt.Errorf("ztoc: unknown compression algorithm %q", algo)
	}
}

func compressionAlgorithmFromEnum(v int8) string {
	switch v {
	case algoUncompressed:
		return CompressionUncompressed
	default:
		return CompressionGzip
	}
}

// Unmarshal parses a Ztoc from its FlatBuffer representation. It mirrors soci's
// deserialization, including sorting file metadata by uncompressed offset and
// recomputing each entry's tar header offset.
func Unmarshal(data []byte) (z *Ztoc, err error) {
	defer func() {
		if r := recover(); r != nil {
			z = nil
			err = fmt.Errorf("ztoc: cannot unmarshal ztoc: %v", r)
		}
	}()

	root, err := getRootTable(data)
	if err != nil {
		return nil, err
	}

	z = &Ztoc{
		Version:                 Version(root.stringField(vo(fZtocVersion))),
		BuildToolIdentifier:     root.stringField(vo(fZtocBuildTool)),
		CompressedArchiveSize:   Offset(scalarInt64(root, fZtocCompressedSize)),
		UncompressedArchiveSize: Offset(scalarInt64(root, fZtocUncompSize)),
	}

	if o := root.offset(vo(fZtocTOC)); o != 0 {
		tocTable := fbTable{buf: data, pos: root.indirect(o + root.pos)}
		z.TOC, err = unmarshalTOC(tocTable)
		if err != nil {
			return nil, err
		}
	}

	if o := root.offset(vo(fZtocCompressionInfo)); o != 0 {
		ci := fbTable{buf: data, pos: root.indirect(o + root.pos)}
		z.CompressionInfo = unmarshalCompressionInfo(ci)
	}
	return z, nil
}

func unmarshalTOC(t fbTable) (TOC, error) {
	var md []FileMetadata
	if o := t.offset(vo(fTOCMetadata)); o != 0 {
		start, n := t.vectorRange(o, sizeUOffsetT)
		md = make([]FileMetadata, n)
		for i := 0; i < n; i++ {
			elem := fbTable{buf: t.buf, pos: t.indirect(start + uint32(i)*sizeUOffsetT)}
			md[i] = unmarshalFileMetadata(elem)
		}
	}

	toc := TOC{FileMetadata: md}
	sort.Slice(toc.FileMetadata, func(i, j int) bool {
		return toc.FileMetadata[i].UncompressedOffset < toc.FileMetadata[j].UncompressedOffset
	})
	next := Offset(0)
	for i := range toc.FileMetadata {
		e := &toc.FileMetadata[i]
		if next > e.UncompressedOffset {
			return toc, fmt.Errorf("ztoc: invalid TOC entry ordering")
		}
		e.TarHeaderOffset = next
		next = alignToTarBlock(e.UncompressedOffset + e.UncompressedSize)
	}
	return toc, nil
}

func unmarshalFileMetadata(t fbTable) FileMetadata {
	me := FileMetadata{
		Name:               t.stringField(vo(fFMName)),
		Type:               t.stringField(vo(fFMType)),
		UncompressedOffset: Offset(scalarInt64(t, fFMUncompOff)),
		UncompressedSize:   Offset(scalarInt64(t, fFMUncompSize)),
		Linkname:           t.stringField(vo(fFMLinkname)),
		Mode:               scalarInt64(t, fFMMode),
		UID:                int(scalarUint32(t, fFMUID)),
		GID:                int(scalarUint32(t, fFMGID)),
		Uname:              t.stringField(vo(fFMUname)),
		Gname:              t.stringField(vo(fFMGname)),
		Devmajor:           scalarInt64(t, fFMDevmajor),
		Devminor:           scalarInt64(t, fFMDevminor),
	}
	var mt time.Time
	_ = mt.UnmarshalText(t.byteVectorField(vo(fFMModTime)))
	me.ModTime = mt

	me.PAXHeaders = map[string]string{}
	if o := t.offset(vo(fFMXattrs)); o != 0 {
		start, n := t.vectorRange(o, sizeUOffsetT)
		for j := 0; j < n; j++ {
			x := fbTable{buf: t.buf, pos: t.indirect(start + uint32(j)*sizeUOffsetT)}
			me.PAXHeaders[x.stringField(vo(fXattrKey))] = x.stringField(vo(fXattrValue))
		}
	}
	return me
}

func unmarshalCompressionInfo(t fbTable) CompressionInfo {
	ci := CompressionInfo{
		MaxSpanID:            SpanID(scalarInt32(t, fCIMaxSpanID)),
		CompressionAlgorithm: CompressionGzip,
	}
	if o := t.offset(vo(fCIAlgorithm)); o != 0 {
		ci.CompressionAlgorithm = compressionAlgorithmFromEnum(t.getInt8(t.pos + o))
	}
	if o := t.offset(vo(fCISpanDigests)); o != 0 {
		start, n := t.vectorRange(o, sizeUOffsetT)
		ci.SpanDigests = make([]digest.Digest, n)
		for i := 0; i < n; i++ {
			s := string(t.byteVector(start + uint32(i)*sizeUOffsetT))
			ci.SpanDigests[i] = digest.Digest(s)
		}
	}
	if o := t.offset(vo(fCICheckpoints)); o != 0 {
		raw := t.byteVector(o + t.pos)
		ci.Checkpoints = make([]byte, len(raw))
		copy(ci.Checkpoints, raw)
	}
	return ci
}

// scalarInt64 reads an int64 field or returns 0 if absent.
func scalarInt64(t fbTable, slot int) int64 {
	if o := t.offset(vo(slot)); o != 0 {
		return t.getInt64(t.pos + o)
	}
	return 0
}

func scalarInt32(t fbTable, slot int) int32 {
	if o := t.offset(vo(slot)); o != 0 {
		return t.getInt32(t.pos + o)
	}
	return 0
}

func scalarUint32(t fbTable, slot int) uint32 {
	if o := t.offset(vo(slot)); o != 0 {
		return t.getUint32(t.pos + o)
	}
	return 0
}

// byteVectorField returns the raw bytes of a string field (no allocation of a
// Go string), or nil if absent.
func (t fbTable) byteVectorField(vtableOffset uint16) []byte {
	if o := t.offset(vtableOffset); o != 0 {
		return t.byteVector(o + t.pos)
	}
	return nil
}
