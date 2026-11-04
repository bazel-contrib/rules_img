package iso9660

import (
	"encoding/binary"
	"fmt"
	"io"
)

// El Torito makes an image bootable: a boot record volume descriptor points at
// a boot catalog, and the catalog's entries point at boot images that are
// ordinary files of the filesystem.

const (
	// maxBootEntries is how many entries fit in the single block of the boot
	// catalog, counting the validation entry and the section header each entry
	// after the first needs.
	maxBootEntries = 20

	// bootInfoTableEnd is the end of the boot information table isolinux
	// expects, which starts at offset 8 of the boot image.
	bootInfoTableEnd = 64

	bootIndicatorBootable = 0x88
	sectionHeaderMore     = 0x90
	sectionHeaderFinal    = 0x91

	// bootCatalogID is recorded in the validation entry. Nothing reads it.
	bootCatalogID = "rules_img"
)

// writeBootRecord writes the boot record volume descriptor, which is how a
// firmware finds the boot catalog.
func (e *emitter) writeBootRecord() {
	b := make([]byte, BlockSize)
	b[0] = 0 // a boot record
	copy(b[1:], "CD001")
	b[6] = 1
	putPadded(b[7:39], "EL TORITO SPECIFICATION", 0)
	putU32LE(b[71:], e.l.bootCatalogLBA)
	e.writeBlocks(e.l.brvdLBA, b)
}

// writeBootCatalog writes the boot catalog: a validation entry, the default
// boot entry, and a section header plus a section entry for every further
// entry.
func (e *emitter) writeBootCatalog() {
	l := e.l
	catalog := make([]byte, BlockSize)
	pos := 0
	put := func(entry []byte) {
		copy(catalog[pos:], entry)
		pos += 32
	}

	put(validationEntry(l.boot[0].Platform))
	put(bootEntry(l.boot[0], 0))
	for i, entry := range l.boot[1:] {
		indicator := byte(sectionHeaderMore)
		if i == len(l.boot)-2 {
			indicator = sectionHeaderFinal
		}
		put(sectionHeader(indicator, entry.Platform, entry.ID))
		put(bootEntry(entry, 0))
	}
	e.writeBlocks(l.bootCatalogLBA, catalog)
}

// validationEntry is the first entry of the catalog. Its checksum makes the
// sixteen bit words of the entry add up to zero.
func validationEntry(platform BootPlatform) []byte {
	entry := make([]byte, 32)
	entry[0] = 1 // a validation entry
	entry[1] = byte(platform)
	putPadded(entry[4:28], bootCatalogID, 0)
	entry[30], entry[31] = 0x55, 0xAA
	var sum uint16
	for i := 0; i < 32; i += 2 {
		sum += binary.LittleEndian.Uint16(entry[i:])
	}
	putU16LE(entry[28:], -sum)
	return entry
}

func sectionHeader(indicator byte, platform BootPlatform, id string) []byte {
	entry := make([]byte, 32)
	entry[0] = indicator
	entry[1] = byte(platform)
	putU16LE(entry[2:], 1) // one section entry follows
	putPadded(entry[4:32], id, 0)
	return entry
}

// bootEntry is the default entry or a section entry; the two have the same
// layout for everything a reader needs.
func bootEntry(e bootEntryPlan, selectionCriteria byte) []byte {
	entry := make([]byte, 32)
	entry[0] = bootIndicatorBootable
	entry[1] = byte(e.Emulation)
	putU16LE(entry[2:], e.LoadSegment)
	putU16LE(entry[6:], bootLoadSize(e))
	putU32LE(entry[8:], e.node.planned.lba)
	entry[12] = selectionCriteria
	return entry
}

// bootLoadSize is how many 512 byte sectors the firmware is told to load: what
// the entry asks for, or the whole image.
func bootLoadSize(e bootEntryPlan) uint16 {
	if e.LoadSize != 0 {
		return e.LoadSize
	}
	sectors := (e.node.size + 511) / 512
	if sectors > 0xFFFF {
		return 0xFFFF
	}
	return uint16(sectors)
}

// bootInfoTable is the table isolinux and its relatives expect at offset 8 of
// their boot image: where the image is, how long it is, and a checksum over
// everything after the table.
//
// The checksum is the sum of the 32 bit little-endian words of the image from
// offset 64 to the end, so the content has to be read once before it is
// written. A patched image is therefore never a reference in a compact stream.
func (e *emitter) bootInfoTable(n *node, pvdLBA uint32) ([]byte, error) {
	rc, err := n.src.open()
	if err != nil {
		return nil, fmt.Errorf("iso9660: %s: %w", n.planned.path, err)
	}
	defer rc.Close()
	checksum, err := bootChecksum(rc, n.size)
	if err != nil {
		return nil, fmt.Errorf("iso9660: %s: computing the boot checksum: %w", n.planned.path, err)
	}

	table := make([]byte, bootInfoTableEnd-8)
	putU32LE(table[0:], pvdLBA)
	putU32LE(table[4:], n.planned.lba)
	putU32LE(table[8:], uint32(n.size))
	putU32LE(table[12:], checksum)
	return table, nil
}

// bootChecksum sums the 32 bit words of the boot image after the table. A
// trailing partial word is zero padded, which is what the boot loaders do.
func bootChecksum(r io.Reader, size int64) (uint32, error) {
	if _, err := io.CopyN(io.Discard, r, bootInfoTableEnd); err != nil {
		return 0, unexpected(err)
	}
	var sum uint32
	var word [4]byte
	remaining := size - bootInfoTableEnd
	buf := make([]byte, 1<<16)
	for remaining > 0 {
		chunk := int(min(int64(len(buf)), remaining))
		if _, err := io.ReadFull(r, buf[:chunk]); err != nil {
			return 0, unexpected(err)
		}
		remaining -= int64(chunk)
		for i := 0; i < chunk; i += 4 {
			clear(word[:])
			copy(word[:], buf[i:min(i+4, chunk)])
			sum += binary.LittleEndian.Uint32(word[:])
		}
	}
	return sum, nil
}

// bootTableFor returns the boot information table of a node that asked for
// one, or nil. It is called while the file data is written, so the digest of
// the image covers the patched bytes.
func (e *emitter) bootTableFor(n *node) ([]byte, error) {
	if !n.planned.patched {
		return nil, nil
	}
	return e.bootInfoTable(n, e.l.pvdLBA)
}
