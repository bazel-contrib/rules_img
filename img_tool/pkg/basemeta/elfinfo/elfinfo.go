// Package elfinfo reads the few pieces of ELF metadata a base image needs from
// a shared library: its SONAME and its machine type.
package elfinfo

import (
	"debug/elf"
	"fmt"
)

// Info describes a shared object.
type Info struct {
	// SONAME is the DT_SONAME of the library, e.g. "libc.so.6". Empty when the
	// library carries none.
	SONAME string
	// Class is 32 or 64, the ELF word size.
	Class int
	// Machine is the ELF machine type, used to tag entries in an ld.so cache.
	Machine elf.Machine
}

// Read returns the ELF metadata of the shared object at path.
func Read(path string) (Info, error) {
	f, err := elf.Open(path)
	if err != nil {
		return Info{}, fmt.Errorf("%s: not an ELF file: %w", path, err)
	}
	defer f.Close()

	// DynString returns the DT_SONAME entries of the .dynamic section. A
	// library without one is legal (and common for a plugin), so an empty
	// result is not an error; the caller falls back to the file name.
	var soname string
	sonames, err := f.DynString(elf.DT_SONAME)
	if err == nil && len(sonames) > 0 {
		soname = sonames[0]
	}

	class := 64
	if f.Class == elf.ELFCLASS32 {
		class = 32
	}

	return Info{SONAME: soname, Class: class, Machine: f.Machine}, nil
}
