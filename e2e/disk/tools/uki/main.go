// Command uki assembles a unified kernel image (UKI): a PE/COFF EFI program made of a
// UEFI stub (systemd-stub) with the kernel, the initrd, the kernel command line and the
// os-release file added as sections.
//
// See https://uapi-group.org/specifications/specs/unified_kernel_image/.
package main

import (
	"archive/tar"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
)

func main() {
	var (
		stub    = flag.String("stub", "", "UEFI stub (e.g. linuxx64.efi.stub)")
		stubTar = flag.String("stub-tar", "", "tar archive that contains the UEFI stub, instead of --stub")
		stubIn  = flag.String("stub-path", "", "path of the UEFI stub inside --stub-tar")
		linux   = flag.String("linux", "", "Linux kernel image (an EFI stub kernel)")
		initrd  = flag.String("initrd", "", "initrd (optional)")
		cmdline = flag.String("cmdline", "", "kernel command line (optional)")
		osrel   = flag.String("osrel", "", "os-release file (optional)")
		output  = flag.String("output", "", "output file")
	)
	flag.Parse()
	if (*stub == "") == (*stubTar == "") || (*stubTar != "" && *stubIn == "") || *linux == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "usage: uki (--stub STUB | --stub-tar TAR --stub-path PATH) --linux KERNEL [--initrd INITRD] [--cmdline CMDLINE] [--osrel FILE] --output OUT")
		os.Exit(2)
	}
	stubData, err := readStub(*stub, *stubTar, *stubIn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "uki:", err)
		os.Exit(1)
	}
	if err := run(stubData, *linux, *initrd, *cmdline, *osrel, *output); err != nil {
		fmt.Fprintln(os.Stderr, "uki:", err)
		os.Exit(1)
	}
}

func run(stub []byte, linuxPath, initrdPath, cmdline, osrelPath, outPath string) error {
	// The order is the one of ukify; the position of a section does not matter to the stub.
	var sections []section
	if osrelPath != "" {
		data, err := os.ReadFile(osrelPath)
		if err != nil {
			return err
		}
		sections = append(sections, section{name: ".osrel", data: data})
	}
	if cmdline != "" {
		// The stub reads the command line as UTF-16 or UTF-8; a trailing NUL is what ukify writes.
		sections = append(sections, section{name: ".cmdline", data: append([]byte(cmdline), 0)})
	}
	if initrdPath != "" {
		data, err := os.ReadFile(initrdPath)
		if err != nil {
			return err
		}
		sections = append(sections, section{name: ".initrd", data: data})
	}
	kernel, err := os.ReadFile(linuxPath)
	if err != nil {
		return err
	}
	sections = append(sections, section{name: ".linux", data: kernel})

	out, err := addSections(stub, sections)
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, out, 0o644)
}

// readStub reads the stub from a file, or from a tar archive (a Debian package, flattened).
func readStub(file, tarFile, inTar string) ([]byte, error) {
	if file != "" {
		return os.ReadFile(file)
	}
	f, err := os.Open(tarFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	want := path.Clean("/" + inTar)
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s not found in %s", inTar, tarFile)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Clean("/"+h.Name) == want {
			return io.ReadAll(tr)
		}
	}
}
