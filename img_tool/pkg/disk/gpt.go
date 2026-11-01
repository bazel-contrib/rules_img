package disk

import (
	"encoding/binary"
	"hash/crc32"
	"unicode/utf16"
)

var le = binary.LittleEndian

func (l *Layout) arraySectors() uint64 {
	return uint64(ceilDiv(gptArrayBytes, int64(l.SectorSize)))
}

// Head returns the bytes in front of the first partition: for a GPT the
// protective MBR, the primary header and the partition entries (that is, up to
// FirstUsableLBA), for an MBR the first sector. It starts at offset zero.
func (l *Layout) Head() []byte {
	if l.Table == MBR {
		return l.encodeMBR()
	}
	ss := l.SectorSize
	head := make([]byte, int(l.FirstUsableLBA)*ss)
	copy(head, l.encodeProtectiveMBR())
	entries := l.encodeEntries()
	copy(head[2*ss:], entries)
	copy(head[ss:], l.encodeHeader(1, l.Sectors-1, 2, entries))
	return head
}

// Tail returns the backup partition table at the end of the disk (GPT only):
// the partition entries and the backup header, and the offset they belong at.
// For an MBR there is no tail.
func (l *Layout) Tail() (data []byte, offset int64) {
	if l.Table != GPT {
		return nil, 0
	}
	ss := l.SectorSize
	arr := l.arraySectors()
	entriesLBA := l.Sectors - 1 - arr
	entries := l.encodeEntries()
	tail := make([]byte, (int(arr)+1)*ss)
	copy(tail, entries)
	copy(tail[int(arr)*ss:], l.encodeHeader(l.Sectors-1, 1, entriesLBA, entries))
	return tail, int64(entriesLBA) * int64(ss)
}

// encodeEntries encodes the partition entry array: 128 entries of 128 bytes,
// padded with zeros to whole sectors.
func (l *Layout) encodeEntries() []byte {
	buf := make([]byte, int(l.arraySectors())*l.SectorSize)
	for i, p := range l.Partitions {
		e := buf[i*gptEntrySize : (i+1)*gptEntrySize]
		t, g := p.TypeGUID.onDisk(), p.GUID.onDisk()
		copy(e[0:16], t[:])
		copy(e[16:32], g[:])
		le.PutUint64(e[32:], p.StartLBA)
		le.PutUint64(e[40:], p.StartLBA+p.Sectors-1)
		le.PutUint64(e[48:], p.Attributes)
		for j, u := range utf16.Encode([]rune(p.Label)) {
			le.PutUint16(e[56+2*j:], u)
		}
	}
	return buf
}

// encodeHeader encodes a GPT header sector.
func (l *Layout) encodeHeader(myLBA, altLBA, entriesLBA uint64, entries []byte) []byte {
	h := make([]byte, l.SectorSize)
	copy(h, "EFI PART")
	le.PutUint32(h[8:], 0x00010000)
	le.PutUint32(h[12:], gptHeaderSize)
	le.PutUint64(h[24:], myLBA)
	le.PutUint64(h[32:], altLBA)
	le.PutUint64(h[40:], l.FirstUsableLBA)
	le.PutUint64(h[48:], l.LastUsableLBA)
	g := l.DiskGUID.onDisk()
	copy(h[56:72], g[:])
	le.PutUint64(h[72:], entriesLBA)
	le.PutUint32(h[80:], gptEntries)
	le.PutUint32(h[84:], gptEntrySize)
	le.PutUint32(h[88:], crc32.ChecksumIEEE(entries))
	le.PutUint32(h[16:], crc32.ChecksumIEEE(h[:gptHeaderSize]))
	return h
}

// encodeProtectiveMBR encodes the MBR in front of a GPT: one partition of type
// 0xEE that spans the disk, so that tools that only know MBR leave it alone.
func (l *Layout) encodeProtectiveMBR() []byte {
	sector := make([]byte, l.SectorSize)
	copy(sector, l.bootCode)
	e := sector[446:462]
	e[0] = 0
	e[1], e[2], e[3] = 0x00, 0x02, 0x00 // CHS of LBA 1
	e[4] = 0xEE
	e[5], e[6], e[7] = 0xFF, 0xFF, 0xFF
	le.PutUint32(e[8:], 1)
	size := l.Sectors - 1
	if size > maxMBRSectors {
		size = maxMBRSectors
	}
	le.PutUint32(e[12:], uint32(size))
	sector[510], sector[511] = 0x55, 0xAA
	return sector
}
