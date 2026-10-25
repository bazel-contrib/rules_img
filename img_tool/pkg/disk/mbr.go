package disk

// encodeMBR encodes the sector with the MBR partition table.
func (l *Layout) encodeMBR() []byte {
	sector := make([]byte, l.SectorSize)
	copy(sector, l.bootCode)
	le.PutUint32(sector[440:], l.DiskSignature)
	for i, p := range l.Partitions {
		e := sector[446+16*i : 446+16*(i+1)]
		if p.Active {
			e[0] = 0x80
		}
		start, end := chs(p.StartLBA), chs(p.StartLBA+p.Sectors-1)
		copy(e[1:4], start[:])
		e[4] = p.MBRType
		copy(e[5:8], end[:])
		le.PutUint32(e[8:], uint32(p.StartLBA))
		le.PutUint32(e[12:], uint32(p.Sectors))
	}
	sector[510], sector[511] = 0x55, 0xAA
	return sector
}

// chs converts an LBA to the cylinder, head and sector bytes of a partition
// entry, assuming the usual geometry of 255 heads and 63 sectors per track.
// Addresses beyond 1023 cylinders are the maximum value, as usual.
func chs(lba uint64) [3]byte {
	const heads, sectors = 255, 63
	cylinder := lba / (heads * sectors)
	if cylinder > 1023 {
		return [3]byte{0xFE, 0xFF, 0xFF}
	}
	head := lba / sectors % heads
	sector := lba%sectors + 1
	return [3]byte{byte(head), byte(sector) | byte(cylinder>>8)<<6, byte(cylinder)}
}
