package iso9660

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// This file checks the UDF structures of an image against ECMA-167 and the
// OSTA UDF profile: the anchors, the two volume descriptor sequences, the
// integrity descriptor, the file set and the File Entries and file identifier
// descriptors that make up the directory tree. Like the ISO 9660 checker next
// to it, it only reads the image.

// udfPresent reports whether the image carries a UDF volume recognition
// sequence.
func udfPresent(c *checker) bool {
	for lba := uint32(systemAreaBlocks); lba < c.blocks && lba < systemAreaBlocks+16; lba++ {
		if string(c.block(lba)[1:6]) == "NSR02" {
			return true
		}
	}
	return false
}

// udfTag is a descriptor tag, as read back.
type udfTag struct {
	id        uint16
	crcLength uint16
	location  uint32
}

// readTag reads and verifies the tag of a descriptor: its checksum, its CRC
// and the block it claims to be in.
func (c *checker) readTag(b []byte, block uint32, what string) udfTag {
	c.t.Helper()
	var sum byte
	for i := 0; i < 16; i++ {
		if i == 4 {
			continue
		}
		sum += b[i]
	}
	if sum != b[4] {
		c.t.Errorf("%s: the tag checksum is %#x, want %#x", what, b[4], sum)
	}
	tag := udfTag{
		id:        binary.LittleEndian.Uint16(b[0:]),
		crcLength: binary.LittleEndian.Uint16(b[10:]),
		location:  binary.LittleEndian.Uint32(b[12:]),
	}
	if version := binary.LittleEndian.Uint16(b[2:]); version != 2 {
		c.t.Errorf("%s: the descriptor version is %d, want 2", what, version)
	}
	if int(16+tag.crcLength) > len(b) {
		c.t.Errorf("%s: the CRC covers %d bytes, more than the descriptor holds", what, tag.crcLength)
		return tag
	}
	if got, want := binary.LittleEndian.Uint16(b[8:]), udfCRC(b[16:16+tag.crcLength]); got != want {
		c.t.Errorf("%s: the CRC is %#x, want %#x", what, got, want)
	}
	if tag.location != block {
		c.t.Errorf("%s: the tag says it is in block %d, but it is in block %d", what, tag.location, block)
	}
	return tag
}

func (c *checker) checkUDF() {
	c.t.Helper()
	// The anchors have to sit in block 256 and in the last block, and both
	// have to point at the same two volume descriptor sequences.
	var mainVDS, reserveVDS uint32
	for _, block := range []uint32{udfAnchorBlock, c.blocks - 1} {
		b := c.block(block)
		tag := c.readTag(b, block, fmt.Sprintf("the anchor in block %d", block))
		if tag.id != tagAnchorPointer {
			c.t.Fatalf("block %d holds the tag %d, want an anchor volume descriptor pointer", block, tag.id)
		}
		c.claim(block, 1, fmt.Sprintf("UDF anchor in block %d", block))
		main := binary.LittleEndian.Uint32(b[20:])
		reserve := binary.LittleEndian.Uint32(b[28:])
		if mainVDS != 0 && (main != mainVDS || reserve != reserveVDS) {
			c.t.Errorf("the anchors disagree about where the volume descriptor sequences are")
		}
		mainVDS, reserveVDS = main, reserve
		if got := binary.LittleEndian.Uint32(b[16:]); got != udfVDSBlocks*BlockSize {
			c.t.Errorf("the main volume descriptor sequence is %d bytes, want %d", got, udfVDSBlocks*BlockSize)
		}
	}

	var partitionStart, partitionBlocks, fileSetLBA uint32
	for _, start := range []uint32{mainVDS, reserveVDS} {
		which := "main"
		if start == reserveVDS {
			which = "reserve"
		}
		c.claim(start, udfVDSBlocks, "UDF "+which+" volume descriptor sequence")
		seen := map[uint16]bool{}
		for i := uint32(0); i < udfVDSBlocks; i++ {
			block := start + i
			b := c.block(block)
			tag := c.readTag(b, block, fmt.Sprintf("the descriptor in block %d", block))
			if tag.id == 0 {
				c.t.Errorf("the %s volume descriptor sequence ends at block %d without a terminator", which, block)
				break
			}
			seen[tag.id] = true
			if got := binary.LittleEndian.Uint32(b[16:]); tag.id != tagTerminatingDescriptor && got != i {
				c.t.Errorf("the descriptor in block %d has the sequence number %d, want %d", block, got, i)
			}
			switch tag.id {
			case tagPartitionDescriptor:
				if got := binary.LittleEndian.Uint32(b[184:]); got != 1 {
					c.t.Errorf("the partition has access type %d, want 1 (read only)", got)
				}
				if got := binary.LittleEndian.Uint16(b[20:]); got&1 == 0 {
					c.t.Error("the partition is not marked as allocated")
				}
				if got := c.entityID(b[24:56]); got != "+NSR02" {
					c.t.Errorf("the partition contents are %q, want %q", got, "+NSR02")
				}
				partitionStart = binary.LittleEndian.Uint32(b[188:])
				partitionBlocks = binary.LittleEndian.Uint32(b[192:])
			case tagLogicalVolumeDescriptor:
				if got := binary.LittleEndian.Uint32(b[212:]); got != BlockSize {
					c.t.Errorf("the logical block size is %d, want %d", got, BlockSize)
				}
				if got := c.entityID(b[216:248]); got != "*OSTA UDF Compliant" {
					c.t.Errorf("the domain identifier is %q, want %q", got, "*OSTA UDF Compliant")
				}
				if got := binary.LittleEndian.Uint16(b[216+24:]); got != udfRevision {
					c.t.Errorf("the domain identifier declares revision %#x, want %#x", got, udfRevision)
				}
				fileSetLBA = binary.LittleEndian.Uint32(b[252:])
				if got := binary.LittleEndian.Uint32(b[264:]); got != 6 {
					c.t.Errorf("the map table is %d bytes, want 6", got)
				}
				if got := binary.LittleEndian.Uint32(b[268:]); got != 1 {
					c.t.Errorf("the volume has %d partition maps, want one", got)
				}
				if b[440] != 1 || b[441] != 6 {
					c.t.Errorf("the partition map is of type %d and %d bytes, want type 1 and 6 bytes", b[440], b[441])
				}
			}
			if tag.id == tagTerminatingDescriptor {
				break
			}
		}
		for _, want := range []uint16{
			tagPrimaryVolumeDescriptor, tagImplementationUseVD, tagPartitionDescriptor,
			tagLogicalVolumeDescriptor, tagUnallocatedSpaceDesc, tagTerminatingDescriptor,
		} {
			if !seen[want] {
				c.t.Errorf("the %s volume descriptor sequence has no descriptor with the tag %d", which, want)
			}
		}
	}

	if partitionStart != udfContentStart {
		c.t.Errorf("the partition starts at block %d, want %d", partitionStart, udfContentStart)
	}
	if partitionStart+partitionBlocks != c.blocks-1 {
		c.t.Errorf("the partition covers blocks %d to %d, want up to the anchor in block %d", partitionStart, partitionStart+partitionBlocks, c.blocks-1)
	}

	// The integrity sequence.
	lvid := c.block(udfIntegrityBlock)
	if tag := c.readTag(lvid, udfIntegrityBlock, "the integrity descriptor"); tag.id != tagIntegrityDescriptor {
		c.t.Errorf("block %d holds the tag %d, want an integrity descriptor", udfIntegrityBlock, tag.id)
	}
	c.claim(udfIntegrityBlock, 2, "UDF integrity sequence")
	if got := binary.LittleEndian.Uint32(lvid[28:]); got != 1 {
		c.t.Errorf("the integrity type is %d, want 1 (closed)", got)
	}
	if got := binary.LittleEndian.Uint32(lvid[84:]); got != partitionBlocks {
		c.t.Errorf("the integrity descriptor says the partition is %d blocks, want %d", got, partitionBlocks)
	}
	term := c.block(udfIntegrityBlock + 1)
	if tag := c.readTag(term, udfIntegrityBlock+1, "the integrity sequence terminator"); tag.id != tagTerminatingDescriptor {
		c.t.Errorf("the integrity sequence is not terminated; block %d holds the tag %d", udfIntegrityBlock+1, tag.id)
	}

	// The file set names the root directory's File Entry.
	fsdBlock := partitionStart + fileSetLBA
	fsd := c.block(fsdBlock)
	// Descriptors inside the partition record their logical block number in
	// the partition, not their block in the volume.
	if tag := c.readTag(fsd, fileSetLBA, "the file set descriptor"); tag.id != tagFileSetDescriptor {
		c.t.Fatalf("block %d holds the tag %d, want a file set descriptor", fsdBlock, tag.id)
	}
	c.claim(fsdBlock, 1, "UDF file set descriptor")
	if got := c.entityID(fsd[416:448]); got != "*OSTA UDF Compliant" {
		c.t.Errorf("the file set's domain identifier is %q, want %q", got, "*OSTA UDF Compliant")
	}
	rootFE := partitionStart + binary.LittleEndian.Uint32(fsd[404:])

	files, dirs := c.walkUDF(partitionStart, rootFE, "", 0, map[uint32]bool{})
	if got := binary.LittleEndian.Uint32(lvid[88+32:]); got != files {
		c.t.Errorf("the integrity descriptor counts %d files, the tree has %d", got, files)
	}
	if got := binary.LittleEndian.Uint32(lvid[88+36:]); got != dirs {
		c.t.Errorf("the integrity descriptor counts %d directories, the tree has %d", got, dirs)
	}
}

// entityID reads the identifier out of a 32 byte entity identifier.
func (c *checker) entityID(b []byte) string {
	return strings.TrimRight(string(b[1:24]), "\x00")
}

// walkUDF walks the UDF directory tree from a File Entry, checking that every
// entry is well formed and that the extents of the regular files are the ones
// the ISO 9660 hierarchy points at. It returns how many files and directories
// it found. Entries that share a File Entry -- hard links -- are only counted
// and checked once, which is what the integrity descriptor counts too.
func (c *checker) walkUDF(partitionStart, feBlock uint32, path string, depth int, visited map[uint32]bool) (files, dirs uint32) {
	c.t.Helper()
	if depth > 64 {
		c.t.Fatalf("the UDF tree is nested too deep at %q", path)
	}
	fe := c.block(feBlock)
	if tag := c.readTag(fe, feBlock-partitionStart, fmt.Sprintf("the File Entry of %q", displayPath(path))); tag.id != tagFileEntry {
		c.t.Fatalf("block %d holds the tag %d, want a File Entry", feBlock, tag.id)
	}
	if visited[feBlock] {
		// Another name for the same file; everything below has been checked.
		return 0, 0
	}
	visited[feBlock] = true
	c.claim(feBlock, 1, fmt.Sprintf("UDF File Entry of %q", displayPath(path)))

	fileType := fe[27]
	if got := binary.LittleEndian.Uint16(fe[20:]); got != 4 {
		c.t.Errorf("%q: the strategy type is %d, want 4", displayPath(path), got)
	}
	if got := binary.LittleEndian.Uint16(fe[34:]) & 7; got != 0 {
		c.t.Errorf("%q: the allocation descriptors are of type %d, want short ones", displayPath(path), got)
	}
	eaLength := binary.LittleEndian.Uint32(fe[168:])
	adLength := binary.LittleEndian.Uint32(fe[172:])
	if 176+eaLength+adLength > BlockSize {
		c.t.Fatalf("%q: the File Entry declares %d bytes of attributes and descriptors", displayPath(path), eaLength+adLength)
	}
	infoLength := binary.LittleEndian.Uint64(fe[56:])

	// The allocation descriptors have to cover exactly the information length.
	var covered uint64
	var firstExtent uint32
	ads := fe[176+eaLength : 176+eaLength+adLength]
	for pos := 0; pos+8 <= len(ads); pos += 8 {
		length := binary.LittleEndian.Uint32(ads[pos:])
		if length>>30 != 0 {
			c.t.Errorf("%q: an extent is of type %d, want a recorded one", displayPath(path), length>>30)
		}
		lbn := binary.LittleEndian.Uint32(ads[pos+4:])
		if pos == 0 {
			firstExtent = partitionStart + lbn
		}
		covered += uint64(length & 0x3FFFFFFF)
	}
	if covered != infoLength {
		c.t.Errorf("%q: the allocation descriptors cover %d bytes, but the information length is %d", displayPath(path), covered, infoLength)
	}

	switch fileType {
	case udfTypeDirectory:
		dirs++
		data := make([]byte, infoLength)
		if err := readFull(c.r, data, int64(firstExtent)*BlockSize); err != nil {
			c.t.Fatalf("reading the UDF directory %q: %v", displayPath(path), err)
		}
		c.claim(firstExtent, blocksFor(int64(infoLength)), fmt.Sprintf("UDF directory data of %q", displayPath(path)))
		sawParent := false
		for pos := 0; pos < len(data); {
			if pos+38 > len(data) {
				c.t.Fatalf("the UDF directory %q ends inside a file identifier descriptor", displayPath(path))
			}
			idLen := int(data[pos+19])
			impUse := int(binary.LittleEndian.Uint16(data[pos+36:]))
			size := (38 + impUse + idLen + 3) / 4 * 4
			if pos+size > len(data) {
				c.t.Fatalf("a file identifier descriptor of the UDF directory %q does not fit", displayPath(path))
			}
			fid := data[pos : pos+size]
			block := firstExtent - partitionStart + uint32(pos/BlockSize)
			if tag := c.readTag(fid, block, fmt.Sprintf("a file identifier descriptor of %q", displayPath(path))); tag.id != tagFileIdentifierDesc {
				c.t.Fatalf("the UDF directory %q holds the tag %d, want a file identifier descriptor", displayPath(path), tag.id)
			}
			if got := binary.LittleEndian.Uint16(fid[16:]); got != 1 {
				c.t.Errorf("a file identifier descriptor of %q has file version %d, want 1", displayPath(path), got)
			}
			characteristics := fid[18]
			child := partitionStart + binary.LittleEndian.Uint32(fid[24:])
			pos += size

			if characteristics&fidParent != 0 {
				if idLen != 0 {
					c.t.Errorf("the parent entry of %q carries an identifier of %d bytes", displayPath(path), idLen)
				}
				if characteristics&fidDirectory == 0 {
					c.t.Errorf("the parent entry of %q is not marked as a directory", displayPath(path))
				}
				sawParent = true
				continue
			}
			name, err := decodeCS0(fid[38+impUse : 38+impUse+idLen])
			if err != nil {
				c.t.Errorf("a name in the UDF directory %q: %v", displayPath(path), err)
				continue
			}
			f, d := c.walkUDF(partitionStart, child, pathOf(path, name), depth+1, visited)
			files += f
			dirs += d
		}
		if !sawParent {
			c.t.Errorf("the UDF directory %q has no parent entry", displayPath(path))
		}
	case udfTypeRegular:
		files++
		// The ISO 9660 hierarchy has to describe the same extent, so the two
		// hierarchies really do share the file data.
		if e, ok := c.entries[path]; ok {
			if infoLength != uint64(e.length) {
				c.t.Errorf("%q is %d bytes under UDF but %d under ISO 9660", path, infoLength, e.length)
			}
			if infoLength > 0 && firstExtent != e.lba {
				c.t.Errorf("%q starts at block %d under UDF but at %d under ISO 9660", path, firstExtent, e.lba)
			}
		} else if c.rockRidge {
			c.t.Errorf("%q is in the UDF tree but not in the ISO 9660 one", path)
		}
	case udfTypeSymlink:
		files++
		if infoLength == 0 {
			c.t.Errorf("the symbolic link %q has no path component list", path)
		}
		c.claim(firstExtent, blocksFor(int64(infoLength)), fmt.Sprintf("UDF symbolic link data of %q", path))
	case udfTypeCharDev, udfTypeBlockDev:
		files++
		if eaLength == 0 {
			c.t.Errorf("the device node %q carries no device specification attribute", path)
			break
		}
		ea := fe[176 : 176+eaLength]
		if tag := c.readTag(ea[:24], feBlock-partitionStart, "an extended attribute header"); tag.id != tagExtendedAttrHeader {
			c.t.Errorf("the attributes of %q start with the tag %d, want an attribute header", path, tag.id)
		}
		if got := binary.LittleEndian.Uint32(ea[24:]); got != 12 {
			c.t.Errorf("the attribute of %q has type %d, want 12 (device specification)", path, got)
		}
	case udfTypeFifo:
		files++
	default:
		c.t.Errorf("%q has the UDF file type %d, which the writer does not produce", path, fileType)
	}
	return files, dirs
}

// decodeCS0 turns a UDF identifier back into a string.
func decodeCS0(b []byte) (string, error) {
	if len(b) == 0 {
		return "", nil
	}
	switch b[0] {
	case 8:
		runes := make([]rune, 0, len(b)-1)
		for _, c := range b[1:] {
			runes = append(runes, rune(c))
		}
		return string(runes), nil
	case 16:
		if len(b)%2 != 1 {
			return "", fmt.Errorf("a sixteen bit identifier of %d bytes has an odd number of characters", len(b))
		}
		return decodeUCS2(b[1:]), nil
	}
	return "", fmt.Errorf("the compression id %d is not one UDF defines", b[0])
}
