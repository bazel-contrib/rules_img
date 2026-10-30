// Package cpio writes cpio archives in the "newc" format (magic 070701) in
// pure Go, using only the standard library. This is the format of a Linux
// initramfs / initrd, which the kernel unpacks into its root file system.
//
// A [Builder] collects a file tree through the same kind of input the
// "img layer" command feeds into a layer (tar headers with content taken from a
// reader, a path or an entry of a tar archive, whole tar streams and
// [io/fs.FS] trees). [Builder.Build] then writes the archive in one go to an
// [io.Writer]. Content is not kept in memory: it is read when the archive is
// written.
//
// # Layout
//
// Entries are written in a depth-first walk of the tree that visits the
// entries of a directory in name order, so a directory always comes before its
// content and the archive does not depend on the order in which entries were
// added. Directories that no entry describes get mode 0755, owned by root,
// because the kernel does not create missing parent directories. The root
// directory is not written. Inode numbers are assigned in archive order,
// starting at 1. The content of each file is contiguous in the archive (see
// [Info.Files]).
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
// bits: files are limited to 4 GiB minus one byte, and ownership and device
// numbers outside of the range are an error rather than silently truncated.
// Modification times are clamped to the range, so that archives with
// timestamps before 1970 or after 2106 can still be converted. Extended attributes are
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

	// defaultSpoolThreshold is how much of a streamed file is kept in memory
	// before it spills to a temporary file.
	defaultSpoolThreshold = 1 << 20

	// memoryBudget is how many bytes of streamed file content the Builder
	// keeps in memory in total. Files beyond it are spooled to a temporary
	// file even if they are smaller than the spool threshold.
	memoryBudget = 64 << 20

	modeTypeMask = 0o170000
	modeDir      = 0o040000
	modeReg      = 0o100000
	modeSymlink  = 0o120000
	modeChar     = 0o020000
	modeBlock    = 0o060000
	modeFifo     = 0o010000
)

// Options configures a [Builder].
type Options struct {
	// Deduplicate merges regular files that have the same content and the same
	// metadata (mode, owner and timestamp): they are stored once, as hard links
	// of a single inode. This is what tar layers do with hard links.
	//
	// Only files added through the Deduplicated methods of the [Builder],
	// [Builder.ImportTar] and [Builder.AddFS] take part, and the first file added
	// is the one that is kept.
	Deduplicate bool

	// DigestFiles records the SHA-256 digest of every regular file's content
	// in [FileLayout.Digest] while the archive is written.
	DigestFiles bool

	// DigestArchive records the SHA-256 digest of the whole archive in
	// [Info.Digest], computed while the archive is written.
	DigestArchive bool

	// SpoolDir is where files that are streamed in through an [io.Reader]
	// and do not fit in memory are stored until Build. The default is
	// [os.TempDir].
	SpoolDir string

	// SpoolThreshold is the largest streamed file kept in memory. Larger
	// files are spooled to SpoolDir. The default is 1 MiB. Independent of
	// this, at most 64 MiB of streamed content is kept in memory in total.
	SpoolThreshold int64
}

func (o *Options) validate() error {
	if o.SpoolThreshold < 0 {
		return fmt.Errorf("cpio: negative spool threshold %d", o.SpoolThreshold)
	}
	if o.SpoolThreshold == 0 {
		o.SpoolThreshold = defaultSpoolThreshold
	}
	return nil
}

// Info describes a written archive.
type Info struct {
	// Size is the size of the archive in bytes.
	Size int64
	// Entries is the number of entries, without the trailer.
	Entries int
	// Digest is the SHA-256 digest of the archive, set if
	// [Options.DigestArchive] is enabled.
	Digest []byte
	// Files lists the location of the content of every regular file that has
	// content, in the order of the archive.
	Files []FileLayout
}

// FileLayout locates the content of a regular file inside the archive. The
// content is stored contiguously: it is the Size bytes starting at Offset.
type FileLayout struct {
	// Path is the slash-separated path of the file, without leading slash.
	// For a set of hard links, it is the path of the member that carries the
	// content: the last one.
	Path string
	// Offset is the byte offset of the first byte of the file's content in
	// the archive.
	Offset int64
	// Size is the size of the file in bytes.
	Size int64
	// Digest is the SHA-256 digest of the content, set if
	// [Options.DigestFiles] is enabled.
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

func checkRange(what string, v int64) error {
	if v < 0 || v > 1<<32-1 {
		return fmt.Errorf("%s %d does not fit in 32 bits", what, v)
	}
	return nil
}
