// Package disktest checks the disk image of the e2e: its GPT, the file systems of the
// partitions and the unified kernel image on the EFI system partition.
package disktest

import (
	"bytes"
	"compress/gzip"
	"debug/pe"
	"encoding/binary"
	"io"
	"os"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

const sectorSize = 512

type partition struct {
	typeGUID string
	name     string
	start    int64
	size     int64
}

// guid formats a GPT GUID (mixed endian on disk) the way it is usually written.
func guid(b []byte) string {
	const hex = "0123456789ABCDEF"
	var sb strings.Builder
	order := []int{3, 2, 1, 0, -1, 5, 4, -1, 7, 6, -1, 8, 9, -1, 10, 11, 12, 13, 14, 15}
	for _, i := range order {
		if i < 0 {
			sb.WriteByte('-')
			continue
		}
		sb.WriteByte(hex[b[i]>>4])
		sb.WriteByte(hex[b[i]&0xf])
	}
	return sb.String()
}

func readGPT(t *testing.T, disk []byte) []partition {
	t.Helper()
	hdr := disk[sectorSize : 2*sectorSize]
	if string(hdr[:8]) != "EFI PART" {
		t.Fatalf("no GPT header at LBA 1: %q", hdr[:8])
	}
	entriesLBA := int64(binary.LittleEndian.Uint64(hdr[72:]))
	count := int(binary.LittleEndian.Uint32(hdr[80:]))
	entrySize := int(binary.LittleEndian.Uint32(hdr[84:]))
	var parts []partition
	for i := 0; i < count; i++ {
		e := disk[entriesLBA*sectorSize+int64(i*entrySize):][:entrySize]
		if bytes.Equal(e[:16], make([]byte, 16)) {
			continue
		}
		first := int64(binary.LittleEndian.Uint64(e[32:]))
		last := int64(binary.LittleEndian.Uint64(e[40:]))
		var name []uint16
		for j := 56; j+1 < entrySize; j += 2 {
			c := binary.LittleEndian.Uint16(e[j:])
			if c == 0 {
				break
			}
			name = append(name, c)
		}
		parts = append(parts, partition{
			typeGUID: guid(e[:16]),
			name:     string(utf16.Decode(name)),
			start:    first * sectorSize,
			size:     (last - first + 1) * sectorSize,
		})
	}
	return parts
}

func TestDiskImage(t *testing.T) {
	path, err := runfiles.Rlocation(os.Getenv("DISK_IMAGE"))
	if err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(disk[510:512], []byte{0x55, 0xaa}) {
		t.Error("the protective MBR has no boot signature")
	}
	parts := readGPT(t, disk)

	want := []struct{ name, typeGUID string }{
		{"ESP", "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"},
		{"root", "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"}, // root, x86-64
		{"var", "4D21B016-B534-45C2-A9FB-5C16E091FD2D"},
		{"home", "933AC7E1-2EB4-4F13-B844-0E14E2AEF915"},
	}
	if len(parts) != len(want) {
		t.Fatalf("got %d partitions, want %d: %+v", len(parts), len(want), parts)
	}
	byName := map[string][]byte{}
	for i, w := range want {
		p := parts[i]
		if p.name != w.name || p.typeGUID != w.typeGUID {
			t.Errorf("partition %d = %s (%s), want %s (%s)", i, p.name, p.typeGUID, w.name, w.typeGUID)
		}
		byName[p.name] = disk[p.start : p.start+p.size]
	}

	// File system signatures.
	if got := string(byName["ESP"][82:90]); got != "FAT32   " {
		t.Errorf("ESP is not FAT32: file system type %q", got)
	}
	if got := binary.LittleEndian.Uint32(byName["root"][1024:]); got != 0xE0F5E1E2 {
		t.Errorf("root is not erofs: magic %#x", got)
	}
	for _, n := range []string{"var", "home"} {
		if got := binary.LittleEndian.Uint16(byName[n][1024+56:]); got != 0xEF53 {
			t.Errorf("%s is not ext4: magic %#x", n, got)
		}
	}

	checkUKI(t, byName["ESP"])
}

// checkUKI finds the unified kernel image in the ESP (a FAT32 layer stores every file
// as one contiguous run of clusters) and checks its sections.
func checkUKI(t *testing.T, esp []byte) {
	t.Helper()
	var f *pe.File
	for off := 0; off+sectorSize <= len(esp); off += sectorSize {
		if esp[off] != 'M' || esp[off+1] != 'Z' {
			continue
		}
		cand, err := pe.NewFile(io.NewSectionReader(bytes.NewReader(esp), int64(off), int64(len(esp)-off)))
		if err != nil {
			t.Logf("PE file candidate at ESP offset %#x: %v", off, err)
			continue
		}
		if cand.Section(".linux") != nil {
			f = cand
			break
		}
	}
	if f == nil {
		t.Fatal("no unified kernel image (a PE file with a .linux section) in the ESP")
	}
	if f.Machine != pe.IMAGE_FILE_MACHINE_AMD64 {
		t.Errorf("UKI machine = %#x, want x86-64", f.Machine)
	}
	section := func(name string) []byte {
		s := f.Section(name)
		if s == nil {
			t.Fatalf("UKI has no %s section", name)
		}
		data, err := s.Data()
		if err != nil {
			t.Fatal(err)
		}
		return data[:s.VirtualSize]
	}

	if cmdline := string(bytes.TrimRight(section(".cmdline"), "\x00")); !strings.Contains(cmdline, "root=PARTLABEL=root") {
		t.Errorf(".cmdline = %q, want root=PARTLABEL=root", cmdline)
	}
	if osrel := string(section(".osrel")); !strings.Contains(osrel, "ID=rules_img_disk") {
		t.Errorf(".osrel = %q", osrel)
	}
	// The kernel is a bzImage, which says so at 0x202 ("HdrS") and is an EFI stub itself.
	kernel := section(".linux")
	if len(kernel) < 0x210 || string(kernel[0x202:0x206]) != "HdrS" {
		t.Error(".linux is not a Linux boot image")
	}
	// The initrd is a gzip-compressed cpio archive with systemd as /init.
	gz, err := gzip.NewReader(bytes.NewReader(section(".initrd")))
	if err != nil {
		t.Fatalf(".initrd is not gzip-compressed: %v", err)
	}
	cpio, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(cpio, []byte("070701")) {
		t.Error(".initrd is not a newc cpio archive")
	}
	for _, name := range []string{"init\x00", "lib/systemd/systemd\x00", "etc/initrd-release\x00"} {
		if !bytes.Contains(cpio, []byte(name)) {
			t.Errorf("initrd has no %q", strings.TrimRight(name, "\x00"))
		}
	}
}
