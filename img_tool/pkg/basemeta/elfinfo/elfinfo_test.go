package elfinfo

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildSharedObject assembles a minimal but well-formed 64-bit little-endian
// ELF shared object whose .dynamic section carries a DT_SONAME.
//
// Writing the bytes by hand keeps the test hermetic: it needs no cross
// toolchain and no checked-in binary fixture, and it exercises exactly the two
// structures Read cares about (the ELF header and the dynamic section).
func buildSharedObject(t *testing.T, soname string) []byte {
	t.Helper()

	const (
		ehdrSize = 64
		phdrSize = 56
		shdrSize = 64
	)

	// Layout: ELF header, one program header, the .dynstr and .dynamic section
	// contents, then the section headers.
	dynstrOff := uint64(ehdrSize + phdrSize)
	// .dynstr starts with the mandatory empty string, then the SONAME.
	dynstr := append([]byte{0}, append([]byte(soname), 0)...)
	sonameIndex := uint64(1)

	dynamicOff := dynstrOff + uint64(len(dynstr))
	var dynamic bytes.Buffer
	writeDyn := func(tag elf.DynTag, val uint64) {
		binary.Write(&dynamic, binary.LittleEndian, uint64(tag))
		binary.Write(&dynamic, binary.LittleEndian, val)
	}
	writeDyn(elf.DT_SONAME, sonameIndex)
	writeDyn(elf.DT_NULL, 0)

	shoff := dynamicOff + uint64(dynamic.Len())
	shstrOff := shoff
	// Section name string table: "", ".dynstr", ".dynamic", ".shstrtab".
	var shstr bytes.Buffer
	shstr.WriteByte(0)
	dynstrNameIdx := uint32(shstr.Len())
	shstr.WriteString(".dynstr\x00")
	dynamicNameIdx := uint32(shstr.Len())
	shstr.WriteString(".dynamic\x00")
	shstrNameIdx := uint32(shstr.Len())
	shstr.WriteString(".shstrtab\x00")
	shoff = shstrOff + uint64(shstr.Len())

	var out bytes.Buffer

	// ELF header.
	out.Write([]byte{0x7f, 'E', 'L', 'F'})
	out.WriteByte(byte(elf.ELFCLASS64))
	out.WriteByte(byte(elf.ELFDATA2LSB))
	out.WriteByte(byte(elf.EV_CURRENT))
	out.WriteByte(byte(elf.ELFOSABI_NONE))
	out.Write(make([]byte, 8)) // ABI version + padding
	binary.Write(&out, binary.LittleEndian, uint16(elf.ET_DYN))
	binary.Write(&out, binary.LittleEndian, uint16(elf.EM_X86_64))
	binary.Write(&out, binary.LittleEndian, uint32(elf.EV_CURRENT))
	binary.Write(&out, binary.LittleEndian, uint64(0))        // entry
	binary.Write(&out, binary.LittleEndian, uint64(ehdrSize)) // phoff
	binary.Write(&out, binary.LittleEndian, shoff)            // shoff
	binary.Write(&out, binary.LittleEndian, uint32(0))        // flags
	binary.Write(&out, binary.LittleEndian, uint16(ehdrSize)) // ehsize
	binary.Write(&out, binary.LittleEndian, uint16(phdrSize)) // phentsize
	binary.Write(&out, binary.LittleEndian, uint16(1))        // phnum
	binary.Write(&out, binary.LittleEndian, uint16(shdrSize)) // shentsize
	binary.Write(&out, binary.LittleEndian, uint16(4))        // shnum
	binary.Write(&out, binary.LittleEndian, uint16(3))        // shstrndx

	// One PT_DYNAMIC program header pointing at the dynamic section.
	binary.Write(&out, binary.LittleEndian, uint32(elf.PT_DYNAMIC))
	binary.Write(&out, binary.LittleEndian, uint32(elf.PF_R))
	binary.Write(&out, binary.LittleEndian, dynamicOff)            // offset
	binary.Write(&out, binary.LittleEndian, dynamicOff)            // vaddr
	binary.Write(&out, binary.LittleEndian, dynamicOff)            // paddr
	binary.Write(&out, binary.LittleEndian, uint64(dynamic.Len())) // filesz
	binary.Write(&out, binary.LittleEndian, uint64(dynamic.Len())) // memsz
	binary.Write(&out, binary.LittleEndian, uint64(8))             // align

	out.Write(dynstr)
	out.Write(dynamic.Bytes())
	out.Write(shstr.Bytes())

	writeSection := func(nameIdx uint32, typ elf.SectionType, off, size, link, entsize uint64) {
		binary.Write(&out, binary.LittleEndian, nameIdx)
		binary.Write(&out, binary.LittleEndian, uint32(typ))
		binary.Write(&out, binary.LittleEndian, uint64(elf.SHF_ALLOC)) // flags
		binary.Write(&out, binary.LittleEndian, off)                   // addr
		binary.Write(&out, binary.LittleEndian, off)                   // offset
		binary.Write(&out, binary.LittleEndian, size)
		binary.Write(&out, binary.LittleEndian, uint32(link))
		binary.Write(&out, binary.LittleEndian, uint32(0)) // info
		binary.Write(&out, binary.LittleEndian, uint64(8)) // addralign
		binary.Write(&out, binary.LittleEndian, entsize)
	}
	// Section 0 is the mandatory null section.
	writeSection(0, elf.SHT_NULL, 0, 0, 0, 0)
	writeSection(dynstrNameIdx, elf.SHT_STRTAB, dynstrOff, uint64(len(dynstr)), 0, 0)
	// .dynamic's link points at .dynstr (section 1), which is where the SONAME
	// index is resolved.
	writeSection(dynamicNameIdx, elf.SHT_DYNAMIC, dynamicOff, uint64(dynamic.Len()), 1, 16)
	writeSection(shstrNameIdx, elf.SHT_STRTAB, shstrOff, uint64(shstr.Len()), 0, 0)

	return out.Bytes()
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadSONAME checks that the SONAME is recovered from the dynamic section.
// This is what the layer's symlink is named after, so a wrong or missing value
// means a binary cannot find the library at runtime.
func TestReadSONAME(t *testing.T) {
	path := writeTemp(t, "libtest.so.1.2.3", buildSharedObject(t, "libtest.so.1"))

	info, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.SONAME != "libtest.so.1" {
		t.Errorf("SONAME = %q, want %q", info.SONAME, "libtest.so.1")
	}
	if info.Class != 64 {
		t.Errorf("Class = %d, want 64", info.Class)
	}
	if info.Machine != elf.EM_X86_64 {
		t.Errorf("Machine = %v, want EM_X86_64", info.Machine)
	}
}

// TestReadRejectsNonELF checks that a file that is not an ELF object fails
// loudly, so a stray text file in `libs` is caught at build time.
func TestReadRejectsNonELF(t *testing.T) {
	path := writeTemp(t, "notalib.so", []byte("#!/bin/sh\necho hello\n"))
	if _, err := Read(path); err == nil {
		t.Fatal("Read accepted a file that is not an ELF object")
	}
}
