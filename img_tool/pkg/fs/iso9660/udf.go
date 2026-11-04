package iso9660

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"
)

// UDF (ECMA-167, as profiled by OSTA UDF 1.02) describes the same file data as
// the ISO 9660 hierarchy, in what is known as a UDF bridge image: the ISO 9660
// volume descriptors come first and the UDF volume recognition sequence
// continues them, so a reader of either format finds what it looks for. The
// UDF partition covers the whole volume, and the File Entry of a regular file
// points at the very extent its directory record points at.
//
// UDF lifts the limits of ISO 9660: names are the real ones, the metadata is
// POSIX, and symbolic links, device nodes, FIFOs and hard links need no
// extension. It costs one logical block per file and directory for the File
// Entry, plus the blocks of the directories' file identifier descriptors.

const (
	// The blocks UDF fixes the use of. The two volume descriptor sequences
	// have to be at least 16 blocks long, the integrity sequence follows them,
	// and an anchor has to sit in block 256 and in the last block.
	udfMainVDSBlock    = 32
	udfReserveVDSBlock = 48
	udfVDSBlocks       = 16
	udfIntegrityBlock  = 64
	udfAnchorBlock     = 256
	// udfContentStart is the first block the image is free to use, and the
	// first block of the UDF partition.
	udfContentStart = udfAnchorBlock + 1

	// udfRevision is the OSTA UDF revision the structures declare.
	udfRevision = 0x0102

	// udfImplementation identifies the writer. Nothing reads it.
	udfImplementation = "*rules_img"
)

// Descriptor tag identifiers.
const (
	tagPrimaryVolumeDescriptor = 1
	tagAnchorPointer           = 2
	tagImplementationUseVD     = 4
	tagPartitionDescriptor     = 5
	tagLogicalVolumeDescriptor = 6
	tagUnallocatedSpaceDesc    = 7
	tagTerminatingDescriptor   = 8
	tagIntegrityDescriptor     = 9
	tagFileSetDescriptor       = 256
	tagFileIdentifierDesc      = 257
	tagExtendedAttrHeader      = 262
	tagFileEntry               = 261
)

// UDF file types.
const (
	udfTypeDirectory = 4
	udfTypeRegular   = 5
	udfTypeBlockDev  = 6
	udfTypeCharDev   = 7
	udfTypeFifo      = 9
	udfTypeSymlink   = 12
)

// File characteristics of a file identifier descriptor.
const (
	fidDirectory = 0x02
	fidParent    = 0x08
)

// Permission bits of a File Entry.
const (
	permOtherExecute = 1 << 0
	permOtherWrite   = 1 << 1
	permOtherRead    = 1 << 2
	permGroupExecute = 1 << 5
	permGroupWrite   = 1 << 6
	permGroupRead    = 1 << 7
	permOwnerExecute = 1 << 10
	permOwnerWrite   = 1 << 11
	permOwnerRead    = 1 << 12
)

// ICB tag flags.
const (
	icbSetUID = 1 << 6
	icbSetGID = 1 << 7
	icbSticky = 1 << 8
)

// udfMaxExtentLength is the largest extent a short allocation descriptor can
// describe, rounded down to a whole number of blocks: the length field is 30
// bits wide, and only the last extent of a file may be a partial block.
const udfMaxExtentLength = (1<<30 - 1) / BlockSize * BlockSize

// udfMaxIdentifier is the length of a file identifier in a file identifier
// descriptor, which is a single byte.
const udfMaxIdentifier = 255

// udfLayout holds the UDF-specific part of the plan.
type udfLayout struct {
	// partitionStart is the first block of the partition; the logical block
	// numbers of the allocation descriptors are relative to it.
	partitionStart uint32
	// partitionBlocks is the length of the partition, which is everything up
	// to the anchor in the last block of the volume.
	partitionBlocks uint32
	fileSetLBA      uint32
	rootFE          uint32
	root            *node
	// files and dirs are what the integrity descriptor reports.
	files, dirs uint32
	// volumeSetID is the unique identifier UDF requires at the start of the
	// volume set identifier.
	volumeSetID string
}

// planUDF sizes the UDF structures: the file identifier descriptors of every
// directory, the path component list of every symbolic link, and the unique
// identifiers.
func (b *Builder) planUDF(l *layout) error {
	u := &udfLayout{root: b.root}
	l.udf = u

	nextID := uint64(16) // UDF reserves the identifiers below 16; the root is 0
	for _, n := range l.nodes {
		if n == b.root {
			n.planned.udfUniqueID = 0
		} else {
			n.planned.udfUniqueID = nextID
			nextID++
		}
		switch n.kind {
		case kindDir:
			u.dirs++
			length, err := udfDirectorySize(n)
			if err != nil {
				return fmt.Errorf("iso9660: %s: %w", displayPath(n.planned.path), err)
			}
			n.planned.udfDataLength = length
		case kindSymlink:
			u.files++
			content, err := udfSymlinkContent(n.target)
			if err != nil {
				return fmt.Errorf("iso9660: %s: %w", displayPath(n.planned.path), err)
			}
			n.planned.udfDataLength = uint32(len(content))
		default:
			u.files++
		}
	}

	// UDF asks for the first eight characters of the volume set identifier to
	// be a unique hexadecimal number. Deriving it from the volume identifier
	// keeps equal inputs producing equal images.
	sum := sha256.Sum256([]byte(b.opts.VolumeID + "\x00" + b.opts.VolumeSetID))
	u.volumeSetID = fmt.Sprintf("%08X%s", binary.BigEndian.Uint32(sum[:4]), b.opts.VolumeSetID)
	return nil
}

func displayPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// udfDirectorySize is the length of the file identifier descriptors of a
// directory: the parent entry plus one per child, each padded to a multiple of
// four bytes. The descriptors are recorded contiguously and may cross a block
// boundary, which every reader handles.
func udfDirectorySize(dir *node) (uint32, error) {
	total := fidSize(0) // the parent entry carries no identifier
	for _, name := range sortedNames(dir) {
		id := cs0(name)
		if len(id) > udfMaxIdentifier {
			return 0, fmt.Errorf("the name %q needs %d bytes as a UDF file identifier, more than the %d one can hold", name, len(id), udfMaxIdentifier)
		}
		total += fidSize(len(id))
	}
	return total, nil
}

// fidSize is the length of a file identifier descriptor with an identifier of
// idLen bytes, padded to a multiple of four.
func fidSize(idLen int) uint32 {
	return uint32((38 + idLen + 3) / 4 * 4)
}

// allocate places the UDF structures that live inside the partition and
// returns the first block after them.
func (u *udfLayout) allocate(l *layout, next uint32) uint32 {
	u.partitionStart = udfContentStart
	u.fileSetLBA = next
	next++
	for _, n := range l.nodes {
		if n.planned.udfDataLength == 0 {
			continue
		}
		n.planned.udfData = next
		next += blocksFor(int64(n.planned.udfDataLength))
	}
	for _, n := range l.nodes {
		n.planned.udfFE = next
		next++
	}
	u.rootFE = u.root.planned.udfFE
	return next
}

// lbn is the logical block number of an absolute block within the partition.
func (u *udfLayout) lbn(block uint32) uint32 { return block - u.partitionStart }

// writeUDF writes everything UDF adds to the image.
func (e *emitter) writeUDF() error {
	e.writeUDFRecognition()
	e.writeUDFVolumeSequence(e.l.udf, udfMainVDSBlock)
	e.writeUDFVolumeSequence(e.l.udf, udfReserveVDSBlock)
	e.writeUDFIntegrity()
	e.writeUDFAnchors()
	e.writeUDFFileSet()
	if err := e.writeUDFDirectories(); err != nil {
		return err
	}
	return e.writeUDFFileEntries()
}

// writeUDFRecognition writes the volume recognition sequence, which continues
// the one the ISO 9660 descriptors started.
func (e *emitter) writeUDFRecognition() {
	l := e.l
	for _, d := range []struct {
		block uint32
		id    string
	}{
		{l.beaLBA, "BEA01"},
		{l.nsrLBA, "NSR02"},
		{l.teaLBA, "TEA01"},
	} {
		b := make([]byte, BlockSize)
		b[0] = 0
		copy(b[1:], d.id)
		b[6] = 1
		e.writeBlocks(d.block, b)
	}
}

// writeUDFVolumeSequence writes one of the two identical volume descriptor
// sequences: the primary volume descriptor, the implementation use descriptor,
// the partition, the logical volume, the unallocated space and a terminator,
// one per block.
func (e *emitter) writeUDFVolumeSequence(u *udfLayout, start uint32) {
	for i, build := range []func(*udfLayout, uint32, uint32) []byte{
		e.udfPrimaryVolumeDescriptor,
		e.udfImplementationUseDescriptor,
		e.udfPartitionDescriptor,
		e.udfLogicalVolumeDescriptor,
		e.udfUnallocatedSpaceDescriptor,
		e.udfTerminatingDescriptor,
	} {
		block := start + uint32(i)
		e.writeBlocks(block, build(u, uint32(i), block))
	}
}

func (e *emitter) udfPrimaryVolumeDescriptor(u *udfLayout, seq, block uint32) []byte {
	b := make([]byte, BlockSize)
	putU32LE(b[16:], seq)
	putU32LE(b[20:], 0) // primary volume descriptor number
	putDString(b[24:56], e.b.opts.VolumeID)
	putU16LE(b[56:], 1) // volume sequence number
	putU16LE(b[58:], 1) // maximum volume sequence number
	putU16LE(b[60:], 2) // interchange level
	putU16LE(b[62:], 2) // maximum interchange level
	putU32LE(b[64:], 1) // character set list
	putU32LE(b[68:], 1) // maximum character set list
	putDString(b[72:200], u.volumeSetID)
	putCharspec(b[200:264])
	putCharspec(b[264:328])
	// No abstract and no copyright extent (bytes 328 to 343).
	putEntityID(b[344:376], 0, udfImplementation, udfEntitySuffix())
	putUDFTime(b[376:388], e.l.time)
	putEntityID(b[388:420], 0, udfImplementation, udfImplementationSuffix())
	putTag(b, tagPrimaryVolumeDescriptor, block, 512-16)
	return b
}

func (e *emitter) udfImplementationUseDescriptor(u *udfLayout, seq, block uint32) []byte {
	b := make([]byte, BlockSize)
	putU32LE(b[16:], seq)
	putEntityID(b[20:52], 0, "*UDF LV Info", udfEntitySuffix())
	// The implementation use area holds the logical volume information.
	putCharspec(b[52:116])
	putDString(b[116:244], e.b.opts.VolumeID)
	putDString(b[244:280], e.b.opts.PublisherID)    // LVInfo1
	putDString(b[280:316], e.b.opts.DataPreparerID) // LVInfo2
	putDString(b[316:352], e.b.opts.ApplicationID)  // LVInfo3
	putEntityID(b[352:384], 0, udfImplementation, udfImplementationSuffix())
	putTag(b, tagImplementationUseVD, block, 512-16)
	return b
}

func (e *emitter) udfPartitionDescriptor(u *udfLayout, seq, block uint32) []byte {
	b := make([]byte, BlockSize)
	putU32LE(b[16:], seq)
	putU16LE(b[20:], 1) // the partition is allocated
	putU16LE(b[22:], 0) // partition number
	putEntityID(b[24:56], 0, "+NSR02", nil)
	// The partition contents use area would hold the tables of a writable
	// partition; a read-only one has none.
	putU32LE(b[184:], 1) // access type: read only
	putU32LE(b[188:], u.partitionStart)
	putU32LE(b[192:], u.partitionBlocks)
	putEntityID(b[196:228], 0, udfImplementation, udfImplementationSuffix())
	putTag(b, tagPartitionDescriptor, block, 512-16)
	return b
}

func (e *emitter) udfLogicalVolumeDescriptor(u *udfLayout, seq, block uint32) []byte {
	b := make([]byte, BlockSize)
	putU32LE(b[16:], seq)
	putCharspec(b[20:84])
	putDString(b[84:212], e.b.opts.VolumeID)
	putU32LE(b[212:], BlockSize)
	putEntityID(b[216:248], 0, "*OSTA UDF Compliant", udfDomainSuffix())
	// The contents use area points at the file set descriptor.
	putLongAD(b[248:264], BlockSize, u.lbn(u.fileSetLBA))
	putU32LE(b[264:], 6) // map table length
	putU32LE(b[268:], 1) // one partition map
	putEntityID(b[272:304], 0, udfImplementation, udfImplementationSuffix())
	// The integrity sequence is the descriptor and its terminator.
	putU32LE(b[432:], 2*BlockSize)
	putU32LE(b[436:], udfIntegrityBlock)
	// A type 1 partition map: one physical partition.
	b[440], b[441] = 1, 6
	putU16LE(b[442:], 1) // volume sequence number
	putU16LE(b[444:], 0) // partition number
	putTag(b, tagLogicalVolumeDescriptor, block, 440+6-16)
	return b
}

func (e *emitter) udfUnallocatedSpaceDescriptor(u *udfLayout, seq, block uint32) []byte {
	b := make([]byte, BlockSize)
	putU32LE(b[16:], seq)
	putU32LE(b[20:], 0) // no unallocated space on a read-only volume
	putTag(b, tagUnallocatedSpaceDesc, block, 24-16)
	return b
}

func (e *emitter) udfTerminatingDescriptor(u *udfLayout, seq, block uint32) []byte {
	b := make([]byte, BlockSize)
	putTag(b, tagTerminatingDescriptor, block, 512-16)
	return b
}

// writeUDFIntegrity writes the logical volume integrity descriptor, which
// records that the volume was closed cleanly, and its terminator.
func (e *emitter) writeUDFIntegrity() {
	u := e.l.udf
	b := make([]byte, BlockSize)
	putUDFTime(b[16:28], e.l.time)
	putU32LE(b[28:], 1) // integrity type: close
	// No further integrity extent.
	putU64LE(b[40:], uint64(16+u.files+u.dirs)) // the next unique identifier
	putU32LE(b[72:], 1)                         // one partition
	putU32LE(b[76:], 46)                        // length of the implementation use area
	putU32LE(b[80:], 0)                         // free space: a read-only volume has none
	putU32LE(b[84:], u.partitionBlocks)
	impUse := b[88:]
	putEntityID(impUse[0:32], 0, udfImplementation, udfImplementationSuffix())
	putU32LE(impUse[32:], u.files)
	putU32LE(impUse[36:], u.dirs)
	putU16LE(impUse[40:], udfRevision) // minimum read revision
	putU16LE(impUse[42:], udfRevision) // minimum write revision
	putU16LE(impUse[44:], udfRevision) // maximum write revision
	putTag(b, tagIntegrityDescriptor, udfIntegrityBlock, 88+46-16)
	e.writeBlocks(udfIntegrityBlock, b)

	term := make([]byte, BlockSize)
	putTag(term, tagTerminatingDescriptor, udfIntegrityBlock+1, 512-16)
	e.writeBlocks(udfIntegrityBlock+1, term)
}

// writeUDFAnchors writes the anchor volume descriptor pointers, which are what
// a reader looks for first: one in block 256 and one in the last block of the
// volume.
func (e *emitter) writeUDFAnchors() {
	for _, block := range []uint32{udfAnchorBlock, e.l.totalBlocks - 1} {
		b := make([]byte, BlockSize)
		putU32LE(b[16:], udfVDSBlocks*BlockSize)
		putU32LE(b[20:], udfMainVDSBlock)
		putU32LE(b[24:], udfVDSBlocks*BlockSize)
		putU32LE(b[28:], udfReserveVDSBlock)
		putTag(b, tagAnchorPointer, block, 512-16)
		e.writeBlocks(block, b)
	}
}

// writeUDFFileSet writes the file set descriptor, which names the root
// directory's File Entry.
func (e *emitter) writeUDFFileSet() {
	u := e.l.udf
	b := make([]byte, BlockSize)
	putUDFTime(b[16:28], e.l.time)
	putU16LE(b[28:], 3) // interchange level
	putU16LE(b[30:], 3) // maximum interchange level
	putU32LE(b[32:], 1) // character set list
	putU32LE(b[36:], 1) // maximum character set list
	putU32LE(b[40:], 0) // file set number
	putU32LE(b[44:], 0) // file set descriptor number
	putCharspec(b[48:112])
	putDString(b[112:240], e.b.opts.VolumeID)
	putCharspec(b[240:304])
	putDString(b[304:336], e.b.opts.VolumeID)
	// No copyright and no abstract file.
	putLongAD(b[400:416], BlockSize, u.lbn(u.rootFE))
	putEntityID(b[416:448], 0, "*OSTA UDF Compliant", udfDomainSuffix())
	// The tag of a descriptor inside the partition records its logical
	// block number in the partition, not its block in the volume.
	putTag(b, tagFileSetDescriptor, u.lbn(u.fileSetLBA), 512-16)
	e.writeBlocks(u.fileSetLBA, b)
}

// writeUDFDirectories writes the file identifier descriptors of every
// directory and the path component list of every symbolic link.
func (e *emitter) writeUDFDirectories() error {
	u := e.l.udf
	for _, n := range e.l.nodes {
		switch n.kind {
		case kindDir:
			data := make([]byte, uint64(blocksFor(int64(n.planned.udfDataLength)))*BlockSize)
			pos := 0
			parent := n.planned.udfFE
			if p := e.parentOf(n); p != nil {
				parent = p.planned.udfFE
			}
			pos += putFID(data[pos:], u.lbn(n.planned.udfData), uint32(pos), fidDirectory|fidParent, nil, u.lbn(parent))
			for _, name := range sortedNames(n) {
				child := n.children[name]
				flags := byte(0)
				if child.kind == kindDir {
					flags |= fidDirectory
				}
				pos += putFID(data[pos:], u.lbn(n.planned.udfData), uint32(pos), flags, cs0(name), u.lbn(child.planned.udfFE))
			}
			e.writeBlocks(n.planned.udfData, data)
		case kindSymlink:
			content, err := udfSymlinkContent(n.target)
			if err != nil {
				return err
			}
			data := make([]byte, uint64(blocksFor(int64(len(content))))*BlockSize)
			copy(data, content)
			e.writeBlocks(n.planned.udfData, data)
		}
	}
	return e.err
}

// parentOf finds the directory a directory is in. Directories are never
// shared, so there is at most one.
func (e *emitter) parentOf(dir *node) *node {
	if d, ok := e.parents[dir]; ok {
		return d
	}
	return nil
}

// putFID writes one file identifier descriptor and returns its length. A
// descriptor may cross a block boundary, so its tag records the block the
// directory's data starts in and the offset the descriptor has in it.
func putFID(b []byte, dataLBN, offset uint32, characteristics byte, id []byte, icbLBN uint32) int {
	size := int(fidSize(len(id)))
	fid := b[:size]
	putU16LE(fid[16:], 1) // file version number
	fid[18] = characteristics
	fid[19] = byte(len(id))
	putLongAD(fid[20:36], BlockSize, icbLBN)
	putU16LE(fid[36:], 0) // no implementation use
	copy(fid[38:], id)
	// The tag records the logical block of the partition the descriptor
	// begins in: the first block of the directory data plus however many
	// whole blocks precede it.
	putTag(fid, tagFileIdentifierDesc, dataLBN+offset/BlockSize, size-16)
	return size
}

// writeUDFFileEntries writes one File Entry per node.
func (e *emitter) writeUDFFileEntries() error {
	for _, n := range e.l.nodes {
		entry, err := e.udfFileEntry(n)
		if err != nil {
			return err
		}
		e.writeBlocks(n.planned.udfFE, entry)
	}
	return e.err
}

// udfFileEntry builds the File Entry of a node: its metadata, the extended
// attribute that carries a device number, and the allocation descriptors of
// its data.
func (e *emitter) udfFileEntry(n *node) ([]byte, error) {
	u := e.l.udf
	b := make([]byte, BlockSize)

	var fileType byte
	switch n.kind {
	case kindDir:
		fileType = udfTypeDirectory
	case kindReg:
		fileType = udfTypeRegular
	case kindSymlink:
		fileType = udfTypeSymlink
	case kindChar:
		fileType = udfTypeCharDev
	case kindBlock:
		fileType = udfTypeBlockDev
	case kindFifo:
		fileType = udfTypeFifo
	}

	// The ICB tag: a single direct entry whose data is described by short
	// allocation descriptors.
	putU32LE(b[16:], 0) // no prior direct entries
	putU16LE(b[20:], 4) // strategy type 4: a single entry
	putU16LE(b[24:], 1) // maximum number of entries
	b[27] = fileType
	putU32LE(b[28:], 0) // the parent ICB is not recorded
	putU16LE(b[32:], 0)
	flags := uint16(0) // short allocation descriptors
	if n.mode&0o4000 != 0 {
		flags |= icbSetUID
	}
	if n.mode&0o2000 != 0 {
		flags |= icbSetGID
	}
	if n.mode&0o1000 != 0 {
		flags |= icbSticky
	}
	putU16LE(b[34:], flags)

	putU32LE(b[36:], n.uid)
	putU32LE(b[40:], n.gid)
	putU32LE(b[44:], udfPermissions(n.mode))
	putU16LE(b[48:], uint16(min(n.planned.nlink, 0xFFFF)))
	putU32LE(b[52:], 0) // record length
	putUDFTime(b[72:84], n.atime)
	putUDFTime(b[84:96], n.mtime)
	putUDFTime(b[96:108], n.ctime)
	putU32LE(b[108:], 1) // checkpoint
	putEntityID(b[128:160], 0, udfImplementation, udfImplementationSuffix())
	putU64LE(b[160:], n.planned.udfUniqueID)

	// Extended attributes: a device node records its device number in one.
	eaLength := 0
	if n.kind == kindChar || n.kind == kindBlock {
		ea := b[176:]
		putU32LE(ea[16:], 48) // implementation use attributes start after ours
		putU32LE(ea[20:], 48) // and so do application use attributes
		putTag(ea[:24], tagExtendedAttrHeader, u.lbn(n.planned.udfFE), 24-16)
		dev := ea[24:]
		putU32LE(dev[0:], 12) // device specification
		dev[4] = 1            // subtype
		putU32LE(dev[8:], 24) // attribute length
		putU32LE(dev[12:], 0) // no implementation use
		putU32LE(dev[16:], n.major)
		putU32LE(dev[20:], n.minor)
		eaLength = 48
	}

	// Allocation descriptors: the data of the node, as recorded extents.
	var length int64
	var firstBlock uint32
	switch n.kind {
	case kindReg:
		length, firstBlock = n.size, n.planned.lba
	case kindDir, kindSymlink:
		length, firstBlock = int64(n.planned.udfDataLength), n.planned.udfData
	}
	ads := b[176+eaLength:]
	adLength := 0
	for remaining, block := length, firstBlock; remaining > 0; {
		extent := min(remaining, udfMaxExtentLength)
		if 176+eaLength+adLength+8 > BlockSize {
			return nil, fmt.Errorf("iso9660: %s: the allocation descriptors do not fit in a File Entry", displayPath(n.planned.path))
		}
		putU32LE(ads[adLength:], uint32(extent))
		putU32LE(ads[adLength+4:], u.lbn(block))
		adLength += 8
		block += blocksFor(extent)
		remaining -= extent
	}

	putU64LE(b[56:], uint64(length))
	putU64LE(b[64:], uint64(blocksFor(length)))
	putU32LE(b[168:], uint32(eaLength))
	putU32LE(b[172:], uint32(adLength))
	putTag(b, tagFileEntry, u.lbn(n.planned.udfFE), 176+eaLength+adLength-16)
	return b, nil
}

// udfPermissions maps a POSIX mode to the permission bits of a File Entry.
// UDF has no setuid, setgid or sticky bit here; they live in the ICB flags.
func udfPermissions(mode uint32) uint32 {
	var p uint32
	for _, m := range []struct {
		posix uint32
		udf   uint32
	}{
		{0o400, permOwnerRead}, {0o200, permOwnerWrite}, {0o100, permOwnerExecute},
		{0o040, permGroupRead}, {0o020, permGroupWrite}, {0o010, permGroupExecute},
		{0o004, permOtherRead}, {0o002, permOtherWrite}, {0o001, permOtherExecute},
	} {
		if mode&m.posix != 0 {
			p |= m.udf
		}
	}
	return p
}

// Component types of the path component list a UDF symbolic link stores.
const (
	udfComponentRoot    = 2
	udfComponentParent  = 3
	udfComponentCurrent = 4
	udfComponentName    = 5
)

// udfSymlinkContent encodes a symbolic link target as the path component list
// UDF stores instead of a plain string.
func udfSymlinkContent(target string) ([]byte, error) {
	var out []byte
	appendComponent := func(kind byte, name string) error {
		id := []byte(nil)
		if name != "" {
			id = cs0(name)
			if len(id) > udfMaxIdentifier {
				return fmt.Errorf("the component %q of the symbolic link target is too long for UDF", name)
			}
		}
		comp := make([]byte, 4+len(id))
		comp[0] = kind
		comp[1] = byte(len(id))
		putU16LE(comp[2:], 0) // component file version number
		copy(comp[4:], id)
		out = append(out, comp...)
		return nil
	}

	rest := target
	if strings.HasPrefix(rest, "/") {
		if err := appendComponent(udfComponentRoot, ""); err != nil {
			return nil, err
		}
		rest = strings.TrimPrefix(rest, "/")
	}
	for _, part := range strings.Split(rest, "/") {
		var err error
		switch part {
		case "":
		case ".":
			err = appendComponent(udfComponentCurrent, "")
		case "..":
			err = appendComponent(udfComponentParent, "")
		default:
			err = appendComponent(udfComponentName, part)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("symbolic link target %q has no components", target)
	}
	if len(out) > BlockSize {
		return nil, fmt.Errorf("symbolic link target %q does not fit in a block", target)
	}
	return out, nil
}

// putTag writes the descriptor tag in front of a structure. The checksum
// covers the tag itself and the CRC the crcLength bytes that follow it.
func putTag(b []byte, id uint16, location uint32, crcLength int) {
	putU16LE(b[0:], id)
	putU16LE(b[2:], 2) // descriptor version: ECMA-167, second edition
	b[4] = 0
	b[5] = 0
	putU16LE(b[6:], 0) // tag serial number
	putU16LE(b[8:], udfCRC(b[16:16+crcLength]))
	putU16LE(b[10:], uint16(crcLength))
	putU32LE(b[12:], location)
	var sum byte
	for i := 0; i < 16; i++ {
		if i == 4 {
			continue
		}
		sum += b[i]
	}
	b[4] = sum
}

// udfCRCTable is the table of the CRC-ITU-T polynomial x^16 + x^12 + x^5 + 1
// that ECMA-167 uses for its descriptors.
var udfCRCTable = func() [256]uint16 {
	var t [256]uint16
	for i := range t {
		crc := uint16(i) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
		t[i] = crc
	}
	return t
}()

func udfCRC(p []byte) uint16 {
	var crc uint16
	for _, c := range p {
		crc = crc<<8 ^ udfCRCTable[byte(crc>>8)^c]
	}
	return crc
}

// putUDFTime writes the twelve byte timestamp of UDF. The time is recorded as
// local time with a zero offset from UTC, which is how every writer records a
// reproducible timestamp.
func putUDFTime(b []byte, t time.Time) {
	t = t.UTC()
	if t.Year() < 1 {
		t = time.Unix(0, 0).UTC()
	}
	if t.Year() > 9999 {
		t = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	}
	putU16LE(b[0:], 0x1000) // type 1, offset zero
	putU16LE(b[2:], uint16(t.Year()))
	b[4] = byte(t.Month())
	b[5] = byte(t.Day())
	b[6] = byte(t.Hour())
	b[7] = byte(t.Minute())
	b[8] = byte(t.Second())
	b[9] = byte(t.Nanosecond() / 10_000_000)
	b[10] = byte(t.Nanosecond() / 100_000 % 100)
	b[11] = byte(t.Nanosecond() / 1_000 % 100)
}

// putEntityID writes a 32 byte entity identifier: flags, a 23 character
// identifier and an 8 byte suffix whose shape depends on the identifier.
func putEntityID(b []byte, flags byte, id string, suffix []byte) {
	clear(b[:32])
	b[0] = flags
	copy(b[1:24], id)
	copy(b[24:32], suffix)
}

// udfDomainSuffix is the suffix of the "*OSTA UDF Compliant" identifier: the
// revision and the flags that would write-protect the volume.
func udfDomainSuffix() []byte {
	s := make([]byte, 8)
	putU16LE(s, udfRevision)
	return s
}

// udfEntitySuffix is the suffix of a UDF-defined identifier: the revision and
// the operating system the volume was written on, which is left unspecified.
func udfEntitySuffix() []byte {
	s := make([]byte, 8)
	putU16LE(s, udfRevision)
	return s
}

// udfImplementationSuffix is the suffix of an implementation identifier: the
// operating system, which is left unspecified so the image does not depend on
// where it was built.
func udfImplementationSuffix() []byte { return make([]byte, 8) }

// putCharspec writes the character set specification UDF uses everywhere.
func putCharspec(b []byte) {
	clear(b[:64])
	b[0] = 0 // CS0
	copy(b[1:], "OSTA Compressed Unicode")
}

// putDString writes a fixed width OSTA CS0 string: the encoded characters at
// the front and the number of bytes they occupy in the last byte of the field.
func putDString(b []byte, s string) {
	clear(b)
	if s == "" {
		return
	}
	room := len(b) - 1
	enc := cs0(s)
	for limit := utf16Len(s); len(enc) > room && limit > 0; limit-- {
		s = truncateUTF16(s, limit-1)
		if s == "" {
			return
		}
		enc = cs0(s)
	}
	if len(enc) > room {
		return
	}
	copy(b, enc)
	b[len(b)-1] = byte(len(enc))
}

// cs0 encodes a string the way UDF stores identifiers: a compression byte
// saying whether the characters are eight or sixteen bits wide, followed by
// the characters.
func cs0(s string) []byte {
	units := utf16.Encode([]rune(s))
	wide := false
	for _, u := range units {
		if u > 0xFF {
			wide = true
			break
		}
	}
	if !wide {
		out := make([]byte, 1+len(units))
		out[0] = 8
		for i, u := range units {
			out[i+1] = byte(u)
		}
		return out
	}
	out := make([]byte, 1+2*len(units))
	out[0] = 16
	for i, u := range units {
		out[1+2*i] = byte(u >> 8)
		out[2+2*i] = byte(u)
	}
	return out
}

// putLongAD writes a long allocation descriptor: a length and a logical block
// in the only partition of the volume.
func putLongAD(b []byte, length, lbn uint32) {
	clear(b[:16])
	putU32LE(b[0:], length)
	putU32LE(b[4:], lbn)
	putU16LE(b[8:], 0) // partition reference number
}
