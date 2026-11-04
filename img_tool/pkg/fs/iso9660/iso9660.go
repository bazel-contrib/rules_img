// Package iso9660 writes ISO 9660 filesystem images in pure Go, using only the
// standard library, with the same structure as the ext4 and fat32 packages next
// to it.
//
// A [Builder] collects a file tree through the same kind of input the
// "img layer" command feeds into a layer (tar headers with content taken from a
// reader or a path, whole tar streams and [io/fs.FS] trees). [Builder.Build]
// then lays the image out and writes it in one go to an [io.WriterAt].
//
// # Extensions
//
// Plain ISO 9660 can only store upper case, 8.3-ish names and no POSIX
// metadata at all, so the interesting part of the format is its extensions.
// Each of them is optional and they can be combined; every one of them
// describes the same file data, which is stored exactly once:
//
//   - Rock Ridge ([Options.RockRidge], SUSP 1.12 plus RRIP 1.12) adds the real
//     names, the mode, the owner, the timestamps, symbolic links, device nodes,
//     FIFOs and hard links. This is what Linux uses.
//   - Joliet ([Options.Joliet]) adds a second directory hierarchy with UCS-2
//     names. This is what Windows uses.
//   - UDF ([Options.UDF], revision 1.02 in the "UDF bridge" layout) adds a
//     third, ECMA-167 directory hierarchy over the same data, with long names
//     and POSIX metadata. This is what macOS and Windows prefer, and what lifts
//     the 4 GiB file size limit of ISO 9660.
//
// El Torito ([Options.Boot]) makes the image bootable by pointing firmware at
// boot images that are ordinary files of the filesystem.
//
// # Layout
//
// The image is planned completely before the first byte is written, so every
// structure is written exactly once at its final location:
//
//   - the 32 KiB system area, then the volume descriptors;
//   - the El Torito boot catalog, the path tables, the directory extents, the
//     Rock Ridge continuation areas and the UDF structures;
//   - the data of every regular file, in the order in which the files were
//     added. Each file occupies one contiguous, block-aligned run, so a file
//     can be read from the image using nothing but its offset and size (see
//     [Info.Files]).
//
// # Sparse output
//
// Nothing is written for blocks that are all zero: the padding between
// structures and all-zero blocks of file data. The image therefore only reads
// back correctly if the target returns zeros for ranges that were never
// written, as a freshly created or truncated file does.
//
// # What ISO 9660 cannot store
//
// Without Rock Ridge and without UDF, the mode, owner and timestamps of an
// entry are lost, symbolic links, device nodes and FIFOs are an error, and hard
// links become separate names for the same data. A file is limited to 4 GiB
// minus one block unless UDF is enabled. Names are mangled into the
// d-characters ISO 9660 allows, which is only visible to a reader that uses
// neither extension.
package iso9660

import (
	"fmt"
	"time"
)

const (
	// BlockSize is the logical block size of the images this package writes.
	// ISO 9660 allows others, but nothing uses them.
	BlockSize = 2048

	// systemAreaBlocks is the 32 KiB in front of the first volume descriptor,
	// which the format reserves for whatever boots the medium.
	systemAreaBlocks = 16

	// maxRecordLength is the largest directory record: its length is a single
	// byte, and it has to be even.
	maxRecordLength = 254

	// maxISOIdentifier and maxDirIdentifier are how long an identifier may be
	// in the primary hierarchy, which is what a reader that uses no extension
	// sees. ISO 9660 level 1 allows 8.3 names; these are the level 2 limits.
	maxISOIdentifier = 30
	maxDirIdentifier = 31

	// maxJolietChars is the Joliet limit of 64 UCS-2 characters;
	// maxJolietLongChars is the relaxed limit every implementation accepts
	// (what mkisofs calls -joliet-long).
	maxJolietChars     = 64
	maxJolietLongChars = 103

	// maxVolumeBlocks is the largest volume the 32 bit block counts of the
	// format can describe, and maxImageSize the same limit in bytes.
	maxVolumeBlocks = 1<<32 - 1
	maxImageSize    = int64(maxVolumeBlocks) * BlockSize
	// maxExtentLength is the largest extent one directory record can describe:
	// its data length is 32 bits wide, and every extent but the last of a file
	// has to be a whole number of logical blocks. A larger file is recorded in
	// several extents (interchange level 3), which is what xorriso does.
	maxExtentLength = (1<<32 - 1) / BlockSize * BlockSize

	defaultSpoolThreshold = 1 << 20
)

// Options configures a [Builder].
type Options struct {
	// VolumeID is the volume identifier, at most 32 characters. The default is
	// "CDROM". It is what a mounted image is usually named after.
	VolumeID string
	// SystemID identifies the system that can act on the system area, at most
	// 32 characters. Empty leaves it blank.
	SystemID string
	// VolumeSetID, PublisherID, DataPreparerID and ApplicationID are the
	// informational fields of the primary volume descriptor, at most 128
	// characters each.
	VolumeSetID    string
	PublisherID    string
	DataPreparerID string
	ApplicationID  string

	// RockRidge adds the Rock Ridge extension, which carries the real names
	// and the POSIX metadata of the entries. Without it, symbolic links,
	// device nodes and FIFOs cannot be stored and are an error.
	RockRidge bool
	// RationalizeRockRidge normalizes the metadata of every entry the way
	// mkisofs -rational-rock does: the owner becomes root, the mode becomes
	// read-only for everyone, the execute bit is kept and shared by all three
	// classes, and the setuid, setgid and sticky bits are cleared. It is what
	// makes an image independent of whoever built it, and it applies to the
	// UDF metadata as well, because that describes the same entries.
	RationalizeRockRidge bool
	// Joliet adds a second directory hierarchy with UCS-2 names, which is what
	// Windows reads.
	Joliet bool
	// JolietLongNames raises the Joliet name limit from the 64 characters of
	// the specification to the 103 every implementation accepts.
	JolietLongNames bool
	// UDF adds UDF 1.02 structures describing the same file data, in the
	// layout known as a UDF bridge image.
	UDF bool

	// Boot are the El Torito boot entries of the image, in order. The first
	// one becomes the default boot entry; the rest become sections of their
	// own. Without any, the image has no boot record.
	Boot []BootEntry

	// Deduplicate stores regular files with the same content and metadata
	// once. With RockRidge they become hard links of each other; without it
	// they are two names for the same extent, which only a reader that counts
	// bytes can tell apart.
	Deduplicate bool

	// Size is the minimum size of the image in bytes. The image is otherwise
	// exactly as large as its content needs; a larger size leaves the extra
	// space unused.
	Size int64

	// Time is used as the volume creation time and for every entry that has no
	// timestamp of its own. The default is the Unix epoch, which keeps images
	// reproducible.
	Time time.Time

	// DigestFiles records the SHA-256 digest of every regular file's content
	// in [FileLayout.Digest] while the image is written.
	DigestFiles bool

	// DigestImage records the SHA-256 digest of the whole image in
	// [Info.Digest], computed while the image is written. The target of Build
	// must then also implement [io.ReaderAt]: the metadata around the file data
	// is read back once, the file data is hashed on its way to the image.
	DigestImage bool

	// SpoolDir is where files that are streamed in through an [io.Reader] and
	// do not fit in memory are stored until Build. The default is
	// [os.TempDir].
	SpoolDir string
	// SpoolThreshold is the largest streamed file kept in memory. Larger files
	// are spooled to SpoolDir. The default is 1 MiB.
	SpoolThreshold int64
}

// BootPlatform is the platform an El Torito boot entry is for.
type BootPlatform uint8

const (
	// BootPlatformX86 is a BIOS boot entry (platform id 0x00).
	BootPlatformX86 BootPlatform = 0x00
	// BootPlatformPowerPC is a PowerPC boot entry (platform id 0x01).
	BootPlatformPowerPC BootPlatform = 0x01
	// BootPlatformMac is a Macintosh boot entry (platform id 0x02).
	BootPlatformMac BootPlatform = 0x02
	// BootPlatformEFI is a UEFI boot entry (platform id 0xEF). The boot image
	// is a FAT filesystem the firmware mounts and reads the boot loader from.
	BootPlatformEFI BootPlatform = 0xEF
)

// BootEmulation is what an El Torito boot entry asks the firmware to emulate
// while it reads the boot image.
type BootEmulation uint8

const (
	// BootNoEmulation loads the boot image as it is. This is what every
	// current boot loader and every UEFI firmware wants.
	BootNoEmulation BootEmulation = 0
	// BootFloppy120, BootFloppy144 and BootFloppy288 present the boot image as
	// a floppy disk of that size in KiB.
	BootFloppy120 BootEmulation = 1
	BootFloppy144 BootEmulation = 2
	BootFloppy288 BootEmulation = 3
	// BootHardDisk presents the boot image as a hard disk.
	BootHardDisk BootEmulation = 4
)

// BootEntry is an El Torito boot entry: it points firmware at a boot image
// that is an ordinary file of the filesystem.
type BootEntry struct {
	// Image is the slash-separated path of the boot image in the filesystem,
	// without a leading slash. It has to be a regular file with content.
	Image string
	// Platform is the firmware the entry is for. The default is
	// BootPlatformX86.
	Platform BootPlatform
	// Emulation is what the firmware emulates. The default, BootNoEmulation,
	// is what a current boot loader expects.
	Emulation BootEmulation
	// LoadSize is how many 512 byte sectors the firmware loads. Zero loads the
	// whole image (clamped to the 65535 sectors the field can hold), which is
	// what a UEFI boot image needs.
	LoadSize uint16
	// LoadSegment is the real mode segment the image is loaded at, for a BIOS
	// entry. Zero means the default of 0x7C0.
	LoadSegment uint16
	// BootInfoTable patches the boot information table isolinux and friends
	// expect into bytes 8 to 64 of the boot image. The content of the image
	// then differs from the file that went in, so its content is always stored
	// in the image itself and never becomes a reference in a compact stream.
	BootInfoTable bool
	// ID is the identifier recorded on the section header of an entry other
	// than the first, at most 28 characters. "EFI" is conventional for a UEFI
	// section but nothing reads it.
	ID string
}

func (o *Options) validate() error {
	if o.VolumeID == "" {
		o.VolumeID = "CDROM"
	}
	for _, f := range []struct {
		name  string
		value string
		limit int
	}{
		{"volume id", o.VolumeID, 32},
		{"system id", o.SystemID, 32},
		{"volume set id", o.VolumeSetID, 128},
		{"publisher id", o.PublisherID, 128},
		{"data preparer id", o.DataPreparerID, 128},
		{"application id", o.ApplicationID, 128},
	} {
		if len(f.value) > f.limit {
			return fmt.Errorf("iso9660: %s %q is longer than %d characters", f.name, f.value, f.limit)
		}
	}
	if o.Size < 0 {
		return fmt.Errorf("iso9660: negative image size %d", o.Size)
	}
	if o.Size > maxImageSize {
		return fmt.Errorf("iso9660: image size %d is larger than the %d bytes the 32 bit block counts of the format can describe", o.Size, maxImageSize)
	}
	if len(o.Boot) > maxBootEntries {
		return fmt.Errorf("iso9660: %d boot entries do not fit in the boot catalog, which holds %d", len(o.Boot), maxBootEntries)
	}
	for i, e := range o.Boot {
		if e.Image == "" {
			return fmt.Errorf("iso9660: boot entry %d has no image", i)
		}
		if len(e.ID) > 28 {
			return fmt.Errorf("iso9660: boot entry %d: id %q is longer than 28 characters", i, e.ID)
		}
	}
	if o.SpoolThreshold == 0 {
		o.SpoolThreshold = defaultSpoolThreshold
	}
	if o.Time.IsZero() {
		o.Time = time.Unix(0, 0)
	}
	return nil
}

// Info describes a built image.
type Info struct {
	// Size is the size of the image in bytes.
	Size int64
	// Blocks is the number of 2048 byte logical blocks of the image.
	Blocks uint32
	// RockRidge, Joliet and UDF report which extensions the image carries.
	RockRidge, Joliet, UDF bool
	// BootCatalog is the logical block of the El Torito boot catalog, or zero
	// if the image is not bootable.
	BootCatalog uint32
	// Digest is the SHA-256 digest of the image, set if [Options.DigestImage]
	// is enabled.
	Digest []byte
	// Files lists the location of the data of every regular file with content,
	// in the order the data was laid out. Files that share their content
	// (hard links, or deduplicated files) appear once, under the first path
	// they were added at.
	Files []FileLayout
}

// FileLayout locates the content of a regular file inside the image. The
// content is stored contiguously: it is the Size bytes starting at Offset.
type FileLayout struct {
	// Path is the slash-separated path of the file, without leading slash.
	Path string
	// Offset is the byte offset of the first byte of the file's content in the
	// image. It is a multiple of [BlockSize].
	Offset int64
	// Size is the size of the file in bytes.
	Size int64
	// Digest is the SHA-256 digest of the content, set if
	// [Options.DigestFiles] is enabled and Patched is false.
	Digest []byte
	// Patched reports that the stored content is not the file that went in:
	// the boot information table of a boot image was written into it. Such a
	// file has no digest, because the content-addressed store does not hold
	// the bytes the image carries.
	Patched bool
}
