package main

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// section is a section to add to a PE file.
type section struct {
	name string
	data []byte
}

const (
	peSignature          = "PE\x00\x00"
	coffHeaderSize       = 20
	sectionHeaderSize    = 40
	pe32PlusMagic        = 0x20b
	scnInitializedData   = 0x00000040
	scnMemRead           = 0x40000000
	maxSectionNameLength = 8
)

func alignUp(v, align uint32) uint32 {
	if align == 0 {
		return v
	}
	return (v + align - 1) / align * align
}

// addSections returns a copy of the PE32+ file pe with the sections appended after its last
// section. Every added section is read-only initialized data; nothing is relocated, since
// the stub reads the sections of its own image by name.
func addSections(pe []byte, sections []section) ([]byte, error) {
	if len(pe) < 0x40 || pe[0] != 'M' || pe[1] != 'Z' {
		return nil, errors.New("not a PE file: missing DOS header")
	}
	peOff := int(binary.LittleEndian.Uint32(pe[0x3c:]))
	if peOff < 0 || peOff+4+coffHeaderSize > len(pe) || string(pe[peOff:peOff+4]) != peSignature {
		return nil, errors.New("not a PE file: missing PE signature")
	}
	coff := peOff + 4
	numSections := int(binary.LittleEndian.Uint16(pe[coff+2:]))
	optSize := int(binary.LittleEndian.Uint16(pe[coff+16:]))
	opt := coff + coffHeaderSize
	if optSize < 96 || opt+optSize > len(pe) {
		return nil, errors.New("PE optional header is truncated")
	}
	if magic := binary.LittleEndian.Uint16(pe[opt:]); magic != pe32PlusMagic {
		return nil, fmt.Errorf("unsupported PE optional header magic %#x (want PE32+)", magic)
	}
	const (
		offSizeOfInitializedData = 8
		offSectionAlignment      = 32
		offFileAlignment         = 36
		offSizeOfImage           = 56
		offSizeOfHeaders         = 60
		offCheckSum              = 64
	)
	sectionAlign := binary.LittleEndian.Uint32(pe[opt+offSectionAlignment:])
	fileAlign := binary.LittleEndian.Uint32(pe[opt+offFileAlignment:])
	sizeOfHeaders := binary.LittleEndian.Uint32(pe[opt+offSizeOfHeaders:])
	if sectionAlign == 0 || fileAlign == 0 {
		return nil, errors.New("PE alignment is zero")
	}

	table := opt + optSize
	if table+numSections*sectionHeaderSize > len(pe) {
		return nil, errors.New("PE section table is truncated")
	}
	var nextVA, rawEnd uint32
	for i := 0; i < numSections; i++ {
		h := pe[table+i*sectionHeaderSize:]
		name := sectionName(h[:maxSectionNameLength])
		for _, s := range sections {
			if s.name == name {
				return nil, fmt.Errorf("the stub already has a %s section", name)
			}
		}
		virtualSize := binary.LittleEndian.Uint32(h[8:])
		va := binary.LittleEndian.Uint32(h[12:])
		rawSize := binary.LittleEndian.Uint32(h[16:])
		rawPtr := binary.LittleEndian.Uint32(h[20:])
		if end := va + virtualSize; end > nextVA {
			nextVA = end
		}
		if end := rawPtr + rawSize; rawPtr != 0 && end > rawEnd {
			rawEnd = end
		}
	}
	// Anything behind the last section (a COFF symbol table, a signature) is dropped:
	// the output is a different file, so a signature would not be valid anyway.
	if int(rawEnd) > len(pe) {
		return nil, errors.New("PE section data is truncated")
	}
	newTableEnd := uint32(table + (numSections+len(sections))*sectionHeaderSize)
	if newTableEnd > sizeOfHeaders {
		return nil, fmt.Errorf("no space for %d more section headers in the PE headers", len(sections))
	}

	out := append([]byte(nil), pe[:rawEnd]...)
	nextVA = alignUp(nextVA, sectionAlign)
	var addedData uint32
	for i, s := range sections {
		if len(s.name) > maxSectionNameLength {
			return nil, fmt.Errorf("section name %q is longer than %d bytes", s.name, maxSectionNameLength)
		}
		size := uint32(len(s.data))
		rawSize := alignUp(size, fileAlign)
		rawPtr := alignUp(uint32(len(out)), fileAlign)
		if uint64(rawPtr)+uint64(rawSize) > 1<<32-1 || uint64(nextVA)+uint64(size) > 1<<32-1 {
			return nil, fmt.Errorf("section %s does not fit into a PE file", s.name)
		}
		out = append(out, make([]byte, int(rawPtr)-len(out))...)
		out = append(out, s.data...)
		out = append(out, make([]byte, int(rawSize-size))...)

		var h [sectionHeaderSize]byte
		copy(h[:maxSectionNameLength], s.name)
		binary.LittleEndian.PutUint32(h[8:], size)
		binary.LittleEndian.PutUint32(h[12:], nextVA)
		binary.LittleEndian.PutUint32(h[16:], rawSize)
		binary.LittleEndian.PutUint32(h[20:], rawPtr)
		binary.LittleEndian.PutUint32(h[36:], scnInitializedData|scnMemRead)
		copy(out[table+(numSections+i)*sectionHeaderSize:], h[:])

		nextVA = alignUp(nextVA+size, sectionAlign)
		addedData += rawSize
	}

	binary.LittleEndian.PutUint16(out[coff+2:], uint16(numSections+len(sections)))
	// The symbol table (if any) was behind the last section and is gone.
	binary.LittleEndian.PutUint32(out[coff+8:], 0)
	binary.LittleEndian.PutUint32(out[coff+12:], 0)
	sizeOfInit := binary.LittleEndian.Uint32(out[opt+offSizeOfInitializedData:])
	binary.LittleEndian.PutUint32(out[opt+offSizeOfInitializedData:], sizeOfInit+addedData)
	binary.LittleEndian.PutUint32(out[opt+offSizeOfImage:], nextVA)
	// The checksum is only checked for drivers and boot-critical DLLs.
	binary.LittleEndian.PutUint32(out[opt+offCheckSum:], 0)
	return out, nil
}

func sectionName(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
