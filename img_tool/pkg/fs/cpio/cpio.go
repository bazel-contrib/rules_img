// Package cpio writes cpio archives in the "newc" format (magic 070701) in
// pure Go, using only the standard library. This is the format of a Linux
// initramfs / initrd, which the kernel unpacks into its root file system.
//
// A [Builder] collects a file tree through the same kind of input the
// "img layer" command feeds into a layer (tar headers with content taken from
// a reader or a path). [Builder.Build] then writes the archive in one go.
//
// # Layout
//
// Entries are written in sorted path order, so a directory always comes before
// its content and the archive does not depend on the order in which entries
// were added. Directories that no entry describes get mode 0755, owned by
// root, because the kernel does not create missing parent directories. The
// root directory is not written. Inode numbers are assigned in archive order,
// starting at 1.
//
// # Hard links
//
// The members of a set of hard links share an inode number and a link count.
// Like GNU cpio does, only the last member carries the content; the earlier
// ones are empty. The kernel opens every member for writing, so the content
// ends up in the one shared file.
//
// # Limits
//
// The format stores sizes, ownership, timestamps and device numbers in 32
// bits: files are limited to 4 GiB minus one byte, and a value outside of the
// range is an error rather than silently truncated. Extended attributes are
// dropped. Sockets cannot be added.
package cpio

import (
	"fmt"
	"io"
)

const (
	magic        = "070701"
	headerSize   = 110
	trailerName  = "TRAILER!!!"
	maxFileSize  = 1<<32 - 1
	maxNameBytes = 4095 // PATH_MAX - 1

	// DefaultSpoolThreshold is the size up to which file content that is read
	// from a reader is kept in memory.
	DefaultSpoolThreshold = 8 << 20

	modeTypeMask = 0o170000
	modeDir      = 0o040000
	modeReg      = 0o100000
	modeSymlink  = 0o120000
	modeChar     = 0o020000
	modeBlock    = 0o060000
	modeFifo     = 0o010000
)

// Options configure a [Builder].
type Options struct {
	// SpoolThreshold is the size up to which the content of a file that is
	// read from an [io.Reader] is kept in memory until Build. Larger content
	// is written to a temporary file. Zero uses [DefaultSpoolThreshold].
	SpoolThreshold int64
	// SpoolDir is the directory of temporary files. Empty uses os.TempDir.
	SpoolDir string
	// DigestFiles records the SHA-256 digest of the content of every regular
	// file in [Info.Files], which a compact stream needs.
	DigestFiles bool
}

// Info describes a written archive.
type Info struct {
	// Size is the size of the archive in bytes.
	Size int64
	// Entries is the number of entries, without the trailer.
	Entries int
	// Files lists the content of the regular files that is not empty, in the
	// order of the archive. A set of hard links is listed once, at the member
	// that carries the content. Only set with [Options.DigestFiles].
	Files []File
}

// File is the location of the content of a regular file in the archive.
type File struct {
	// Offset and Size are the position of the content in the archive.
	Offset, Size int64
	// Digest is the SHA-256 digest of the content.
	Digest []byte
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func pad4(n int64) int64 { return (4 - n%4) % 4 }

func checkRange(what, name string, v int64) error {
	if v < 0 || v > 1<<32-1 {
		return fmt.Errorf("cpio: %s: %s %d does not fit in 32 bits", name, what, v)
	}
	return nil
}
