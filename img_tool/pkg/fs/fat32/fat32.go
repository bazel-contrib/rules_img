// Package fat32 writes FAT32 filesystem images in pure Go, using only the
// standard library, with the same structure as the ext4 package next to it.
//
// A [Builder] collects a file tree through the same kind of input the
// "img layer" command feeds into a layer (tar headers with content taken from a
// reader or a path, whole tar streams and [io/fs.FS] trees). [Builder.Build]
// then lays the filesystem out and writes it in one go to an [io.WriterAt].
//
// # Layout
//
// The filesystem is planned completely before the first byte is written, so
// every structure is written exactly once at its final location:
//
//   - The reserved area (boot sector, FSInfo, their backups) and the two copies
//     of the FAT come first.
//   - The data region starts with the root directory and all other
//     directories, followed by the data of every regular file in the order in
//     which the files were added. Each file occupies one contiguous run of
//     clusters, so a file can be read from the image using nothing but its
//     offset and size (see [Info.Files]), and the data region is not
//     fragmented.
//   - The first data cluster is aligned to the cluster size, so every file
//     starts at a multiple of the cluster size.
//
// FAT32 volumes need at least 65525 clusters to be recognized as FAT32, so
// the smallest image is about 33 MiB with the smallest clusters (512 bytes).
// Unless [Options.Size] asks for a larger image, the filesystem has no more
// clusters than that or than the content needs.
//
// # Sparse output
//
// Nothing is written for regions that are all zero: the free part of the FATs
// and of the data region, and all-zero clusters of file data. The image
// therefore only reads back correctly if the target returns zeros for ranges
// that were never written, as a freshly created or truncated file does.
//
// # What FAT cannot store
//
// FAT has no symbolic links, device nodes, FIFOs, hard links, ownership,
// permissions (other than a read-only flag, set for files without the owner
// write bit) or extended attributes. Adding a symbolic link, device node or
// FIFO is an error, a hard link becomes a copy of the file, and the rest is
// dropped. Timestamps have a resolution of 2 seconds, lie between 1980 and
// 2107 (values outside are clamped) and are stored as UTC. Names are stored as
// long file names (UTF-16) with a generated short name, are case insensitive,
// and may not contain the characters " * / : < > ? \ | or control characters.
// Files are limited to 4 GiB minus one byte and directories to 65536 entries.
package fat32

import (
	"fmt"
	"time"
)

const (
	minSectorSize     = 512
	maxSectorSize     = 4096
	defaultSectorSize = 512

	// maxClusterSize is the largest cluster size that every implementation
	// supports.
	maxClusterSize = 32768

	reservedSectors = 32 // sectors before the first FAT, as recommended for FAT32
	numFATs         = 2

	rootCluster   = 2
	fsInfoSector  = 1
	backupSector  = 6
	maxNameLength = 255 // UTF-16 code units of a long file name

	// minClusters is the number of clusters a volume needs so that it is
	// recognized as FAT32: more than 65524, plus the margin the specification
	// recommends to stay clear of the boundary.
	minClusters = 65525 + 16
	// maxClusters is the highest number of clusters FAT32 can address.
	maxClusters = 0x0FFFFFF4

	maxFileSize   = 1<<32 - 1
	maxDirEntries = 1 << 16 // a directory is at most 2^21 bytes

	defaultSpoolThreshold = 1 << 20
)

// Options configures a [Builder].
type Options struct {
	// SectorSize is the number of bytes per sector: 512, 1024, 2048 or 4096.
	// The default is 512.
	SectorSize int

	// ClusterSize is the size of a cluster in bytes: a power of two from the
	// sector size up to 32768. The default follows the table of the Microsoft
	// specification, which uses 512 byte clusters up to 260 MB, 4 KiB up to
	// 8 GiB, and larger clusters beyond.
	ClusterSize int

	// Size is the minimum size of the image in bytes. The filesystem is
	// otherwise as small as FAT32 allows (see the package documentation) and
	// as the content needs. When Size is larger, the filesystem is extended to
	// at least that size (to the next cluster) and the extra space is free.
	Size int64

	// Time is used as the volume creation time and for every file that has
	// no timestamp of its own. The default is the Unix epoch, which is clamped
	// to 1 January 1980 and keeps images reproducible.
	Time time.Time

	// VolumeID is the volume serial number. If zero, one is derived from the
	// metadata of the content, so equal inputs produce equal images.
	VolumeID uint32

	// Label is the volume label: at most 11 upper case ASCII characters,
	// digits or the characters allowed in short names, and spaces. Without a
	// label, the volume has none ("NO NAME").
	Label string

	// DigestFiles records the SHA-256 digest of every regular file's content
	// in [FileLayout.Digest] while the image is written.
	DigestFiles bool

	// SpoolDir is where files that are streamed in through an [io.Reader]
	// and do not fit in memory are stored until Build. The default is
	// [os.TempDir].
	SpoolDir string

	// SpoolThreshold is the largest streamed file kept in memory. Larger
	// files are spooled to SpoolDir. The default is 1 MiB.
	SpoolThreshold int64
}

func (o *Options) validate() error {
	if o.SectorSize == 0 {
		o.SectorSize = defaultSectorSize
	}
	if o.SectorSize < minSectorSize || o.SectorSize > maxSectorSize || o.SectorSize&(o.SectorSize-1) != 0 {
		return fmt.Errorf("fat32: invalid sector size %d: must be 512, 1024, 2048 or 4096", o.SectorSize)
	}
	if o.ClusterSize != 0 {
		if o.ClusterSize < o.SectorSize || o.ClusterSize > maxClusterSize || o.ClusterSize&(o.ClusterSize-1) != 0 {
			return fmt.Errorf("fat32: invalid cluster size %d: must be a power of two from the sector size (%d) to %d", o.ClusterSize, o.SectorSize, maxClusterSize)
		}
	}
	if o.Size < 0 {
		return fmt.Errorf("fat32: negative image size %d", o.Size)
	}
	if err := validateLabel(o.Label); err != nil {
		return err
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
	// SectorSize and ClusterSize are in bytes.
	SectorSize, ClusterSize int
	// Clusters is the number of data clusters of the filesystem; UsedClusters
	// of them are in use.
	Clusters, UsedClusters uint32
	// VolumeID is the serial number of the volume.
	VolumeID uint32
	// Files lists the location of the data of every regular file with
	// content, in the order the data was laid out.
	Files []FileLayout
}

// FileLayout locates the content of a regular file inside the image. The
// content is stored contiguously: it is the Size bytes starting at Offset.
type FileLayout struct {
	// Path is the slash-separated path of the file, without leading slash.
	Path string
	// Offset is the byte offset of the first byte of the file's content in
	// the image.
	Offset int64
	// Size is the size of the file in bytes.
	Size int64
	// Digest is the SHA-256 digest of the content, set if
	// [Options.DigestFiles] is enabled.
	Digest []byte
}
