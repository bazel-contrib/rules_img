package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"io"
	"testing"
)

// minimalPE builds a PE32+ file with a single .text section, like a small EFI stub.
func minimalPE() []byte {
	const (
		peOff    = 0x40
		fileAl   = 0x200
		sectionA = 0x1000
	)
	b := make([]byte, 0x400)
	copy(b, "MZ")
	binary.LittleEndian.PutUint32(b[0x3c:], peOff)
	copy(b[peOff:], peSignature)
	coff := peOff + 4
	binary.LittleEndian.PutUint16(b[coff:], 0x8664) // x86-64
	binary.LittleEndian.PutUint16(b[coff+2:], 1)
	// A symbol table behind the sections, which is dropped.
	binary.LittleEndian.PutUint32(b[coff+8:], 0x3f0)
	binary.LittleEndian.PutUint32(b[coff+12:], 2)
	binary.LittleEndian.PutUint16(b[coff+16:], 240)
	binary.LittleEndian.PutUint16(b[coff+18:], 0x22)
	opt := coff + coffHeaderSize
	binary.LittleEndian.PutUint16(b[opt:], pe32PlusMagic)
	binary.LittleEndian.PutUint32(b[opt+32:], sectionA)
	binary.LittleEndian.PutUint32(b[opt+36:], fileAl)
	binary.LittleEndian.PutUint32(b[opt+56:], 0x2000)
	binary.LittleEndian.PutUint32(b[opt+60:], 0x200)
	binary.LittleEndian.PutUint16(b[opt+68:], 10) // EFI application
	binary.LittleEndian.PutUint32(b[opt+108:], 16)
	table := opt + 240
	copy(b[table:], ".text")
	binary.LittleEndian.PutUint32(b[table+8:], 0x100)
	binary.LittleEndian.PutUint32(b[table+12:], 0x1000)
	binary.LittleEndian.PutUint32(b[table+16:], 0x200)
	binary.LittleEndian.PutUint32(b[table+20:], 0x200)
	binary.LittleEndian.PutUint32(b[table+36:], 0x60000020)
	copy(b[0x200:], "stub code")
	return b[:0x400]
}

func TestAddSections(t *testing.T) {
	in := minimalPE()
	kernel := bytes.Repeat([]byte{0xAB}, 5000)
	out, err := addSections(in, []section{
		{name: ".cmdline", data: []byte("console=ttyS0\x00")},
		{name: ".linux", data: kernel},
	})
	if err != nil {
		t.Fatal(err)
	}
	f, err := pe.NewFile(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("output is not a valid PE file: %v", err)
	}
	if len(f.Sections) != 3 {
		t.Fatalf("got %d sections, want 3", len(f.Sections))
	}
	want := map[string][]byte{".cmdline": []byte("console=ttyS0\x00"), ".linux": kernel}
	var prevEnd uint32 = 0x1100
	for _, s := range f.Sections[1:] {
		if s.VirtualAddress < prevEnd || s.VirtualAddress%0x1000 != 0 {
			t.Errorf("%s: bad virtual address %#x (previous section ends at %#x)", s.Name, s.VirtualAddress, prevEnd)
		}
		prevEnd = s.VirtualAddress + s.VirtualSize
		data, err := io.ReadAll(s.Open())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(data, want[s.Name]) || uint32(len(want[s.Name])) != s.VirtualSize {
			t.Errorf("%s: wrong content", s.Name)
		}
		if s.Offset%0x200 != 0 {
			t.Errorf("%s: raw data not file aligned", s.Name)
		}
	}
	if f.FileHeader.PointerToSymbolTable != 0 || f.FileHeader.NumberOfSymbols != 0 {
		t.Error("the symbol table pointer of the dropped symbol table is still set")
	}
	opt := f.OptionalHeader.(*pe.OptionalHeader64)
	if opt.SizeOfImage != alignUp(prevEnd, 0x1000) {
		t.Errorf("SizeOfImage = %#x, want %#x", opt.SizeOfImage, alignUp(prevEnd, 0x1000))
	}
	// The stub itself is unchanged.
	if !bytes.Equal(out[0x200:0x400], in[0x200:0x400]) {
		t.Error("the .text section changed")
	}
}

func TestAddSectionsRejectsDuplicates(t *testing.T) {
	out, err := addSections(minimalPE(), []section{{name: ".text", data: []byte("x")}})
	if err == nil {
		t.Fatalf("expected an error, got %d bytes", len(out))
	}
}

func TestAddSectionsRejectsNoHeaderSpace(t *testing.T) {
	var s []section
	for i := 0; i < 4; i++ {
		s = append(s, section{name: ".a" + string(rune('0'+i)), data: []byte("x")})
	}
	if _, err := addSections(minimalPE(), s); err == nil {
		t.Fatal("expected an error: SizeOfHeaders is 0x200 and the table has no room")
	}
}
