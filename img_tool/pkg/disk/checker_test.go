package disk

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
	"unicode/utf16"
)

// This file contains an independent reader and checker for the tables the
// package writes. It only relies on the on-disk formats (UEFI specification,
// MBR), not on the encoder's helpers.

type gptEntry struct {
	typeGUID, guid string
	start, end     uint64
	attrs          uint64
	name           string
}

func canonical(b []byte) string {
	// GPT stores the first three fields little endian.
	r := []byte{b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15]}
	return GUID(r).String()
}

// image builds the bytes of a whole disk from a layout, with nothing in the
// partitions.
func image(t *testing.T, l *Layout) []byte {
	t.Helper()
	disk := make([]byte, l.Size())
	copy(disk, l.Head())
	if tail, off := l.Tail(); tail != nil {
		copy(disk[off:], tail)
	}
	return disk
}

// checkGPT parses and verifies a GPT disk and returns its entries.
func checkGPT(t *testing.T, disk []byte, ss int, alignment int64) (header []byte, entries []gptEntry) {
	t.Helper()
	sectors := uint64(len(disk) / ss)
	arraySectors := uint64(16384 / ss)

	// protective MBR
	if disk[510] != 0x55 || disk[511] != 0xAA {
		t.Fatal("no MBR signature in the protective MBR")
	}
	pm := disk[446:462]
	if pm[4] != 0xEE || binary.LittleEndian.Uint32(pm[8:]) != 1 {
		t.Errorf("protective MBR entry: % x", pm)
	}
	wantSize := sectors - 1
	if wantSize > 0xFFFFFFFF {
		wantSize = 0xFFFFFFFF
	}
	if got := uint64(binary.LittleEndian.Uint32(pm[12:])); got != wantSize {
		t.Errorf("protective MBR covers %d sectors, want %d", got, wantSize)
	}

	check := func(name string, lba, wantMy, wantAlt, wantEntriesLBA uint64) []byte {
		h := disk[lba*uint64(ss) : lba*uint64(ss)+uint64(ss)]
		if string(h[:8]) != "EFI PART" {
			t.Fatalf("%s header: signature %q", name, h[:8])
		}
		le := binary.LittleEndian
		if le.Uint32(h[8:]) != 0x00010000 || le.Uint32(h[12:]) != 92 {
			t.Errorf("%s header: revision or size wrong", name)
		}
		crc := le.Uint32(h[16:])
		cp := append([]byte(nil), h[:92]...)
		copy(cp[16:20], []byte{0, 0, 0, 0})
		if got := crc32.ChecksumIEEE(cp); got != crc {
			t.Errorf("%s header: CRC %#x, want %#x", name, crc, got)
		}
		if le.Uint64(h[24:]) != wantMy || le.Uint64(h[32:]) != wantAlt || le.Uint64(h[72:]) != wantEntriesLBA {
			t.Errorf("%s header: LBAs %d/%d/%d, want %d/%d/%d", name, le.Uint64(h[24:]), le.Uint64(h[32:]), le.Uint64(h[72:]), wantMy, wantAlt, wantEntriesLBA)
		}
		if le.Uint64(h[40:]) != 2+arraySectors || le.Uint64(h[48:]) != sectors-arraySectors-2 {
			t.Errorf("%s header: usable range %d..%d", name, le.Uint64(h[40:]), le.Uint64(h[48:]))
		}
		if le.Uint32(h[80:]) != 128 || le.Uint32(h[84:]) != 128 {
			t.Errorf("%s header: %d entries of %d bytes", name, le.Uint32(h[80:]), le.Uint32(h[84:]))
		}
		arr := disk[wantEntriesLBA*uint64(ss) : (wantEntriesLBA+arraySectors)*uint64(ss)]
		if got := crc32.ChecksumIEEE(arr[:128*128]); got != le.Uint32(h[88:]) {
			t.Errorf("%s header: entries CRC %#x, want %#x", name, le.Uint32(h[88:]), got)
		}
		return h
	}
	header = check("primary", 1, 1, sectors-1, 2)
	backup := check("backup", sectors-1, sectors-1, 1, sectors-1-arraySectors)
	if !bytes.Equal(header[56:72], backup[56:72]) {
		t.Error("the disk GUID differs between the headers")
	}
	primaryEntries := disk[2*uint64(ss) : (2+arraySectors)*uint64(ss)]
	backupEntries := disk[(sectors-1-arraySectors)*uint64(ss) : (sectors-1)*uint64(ss)]
	if !bytes.Equal(primaryEntries, backupEntries) {
		t.Error("the backup entries differ from the primary entries")
	}

	first, last := binary.LittleEndian.Uint64(header[40:]), binary.LittleEndian.Uint64(header[48:])
	var prevEnd uint64
	for i := 0; i < 128; i++ {
		e := primaryEntries[i*128 : (i+1)*128]
		if bytes.Equal(e[:16], make([]byte, 16)) {
			if !bytes.Equal(e, make([]byte, 128)) {
				t.Errorf("entry %d has no type but data", i)
			}
			continue
		}
		var name []uint16
		for j := 0; j < 36; j++ {
			u := binary.LittleEndian.Uint16(e[56+2*j:])
			if u == 0 {
				break
			}
			name = append(name, u)
		}
		ent := gptEntry{
			typeGUID: canonical(e[0:16]), guid: canonical(e[16:32]),
			start: binary.LittleEndian.Uint64(e[32:]), end: binary.LittleEndian.Uint64(e[40:]),
			attrs: binary.LittleEndian.Uint64(e[48:]), name: string(utf16.Decode(name)),
		}
		if ent.start < first || ent.end > last || ent.end < ent.start {
			t.Errorf("entry %d: %d..%d is outside %d..%d", i, ent.start, ent.end, first, last)
		}
		if ent.start < prevEnd {
			t.Errorf("entry %d overlaps the previous one", i)
		}
		if (int64(ent.start)*int64(ss))%alignment != 0 {
			t.Errorf("entry %d starts at LBA %d, not aligned to %d", i, ent.start, alignment)
		}
		prevEnd = ent.end + 1
		entries = append(entries, ent)
	}
	return header, entries
}

type mbrEntry struct {
	active     bool
	typ        byte
	start, len uint32
	chsStart   [3]byte
	chsEnd     [3]byte
}

func checkMBR(t *testing.T, disk []byte) (entries []mbrEntry, signature uint32) {
	t.Helper()
	if disk[510] != 0x55 || disk[511] != 0xAA {
		t.Fatal("no MBR signature")
	}
	signature = binary.LittleEndian.Uint32(disk[440:])
	if disk[444] != 0 || disk[445] != 0 {
		t.Error("the two bytes after the disk signature are not zero")
	}
	var prevEnd uint32
	for i := 0; i < 4; i++ {
		e := disk[446+16*i : 446+16*(i+1)]
		if bytes.Equal(e, make([]byte, 16)) {
			continue
		}
		ent := mbrEntry{
			active: e[0] == 0x80, typ: e[4],
			start: binary.LittleEndian.Uint32(e[8:]), len: binary.LittleEndian.Uint32(e[12:]),
		}
		if e[0] != 0 && e[0] != 0x80 {
			t.Errorf("entry %d: boot flag %#x", i, e[0])
		}
		copy(ent.chsStart[:], e[1:4])
		copy(ent.chsEnd[:], e[5:8])
		if ent.start < prevEnd {
			t.Errorf("entry %d overlaps the previous one", i)
		}
		prevEnd = ent.start + ent.len
		entries = append(entries, ent)
	}
	return entries, signature
}
