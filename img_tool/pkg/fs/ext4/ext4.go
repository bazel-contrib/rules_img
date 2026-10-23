// Package ext4 writes ext4 filesystem images in pure Go, using only the
// standard library.
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
//   - All per-group metadata (group descriptors, bitmaps and inode tables of
//     every group) is placed at the start of the image (flex_bg with a flex
//     group spanning the whole filesystem), followed by directory blocks,
//     symlink targets, extent tree blocks and extended attribute blocks.
//   - The data of every regular file follows, in the order in which the files
//     were added. Each file occupies one contiguous run of blocks, so a file can
//     be read from the image using nothing but its offset and size (see
//     [Info.Files]). The only reason a file needs more than one extent is the
//     32768 block limit of a single ext4 extent; those extents are adjacent.
//   - Backup superblocks and group descriptors are not written (sparse_super2
//     with no backup groups). They would otherwise sit in the middle of the
//     data run of large files.
//
// The filesystem is as small as the content allows unless [Options.Size] asks
// for a larger image, in which case the additional space is free.
//
// # Sparse output
//
// Nothing is written for regions that are all zero: unused inode table space,
// the bitmaps of empty groups (marked uninitialized in their group descriptor)
// and all-zero blocks of file data. The image therefore only reads back
// correctly if the target returns zeros for ranges that were never written, as
// a freshly created or truncated file does. Use [Options] and a sparse-aware
// file to keep the image small on disk.
//
// # Feature set
//
// The image uses: filetype, extents, flex_bg, sparse_super2, ext_attr,
// large_file, huge_file, dir_nlink, extra_isize and metadata_csum (crc32c).
// It has no journal, directories are linear (no htree), and images are
// limited to 2^32 blocks.
package ext4

import (
	"fmt"
	"time"
)

const (
	minBlockSize     = 1024
	maxBlockSize     = 65536
	defaultBlockSize = 4096

	inodeSize  = 256
	extraIsize = 32 // sizeof(ext4_inode) beyond the classic 128 bytes
	descSize   = 32 // group descriptor size without the 64bit feature

	rootIno  = 2
	firstIno = 11 // first non-reserved inode; lost+found when present

	// maxExtentLen is the longest initialized extent.
	maxExtentLen = 32768

	// maxNameLen is the longest name of a directory entry.
	maxNameLen = 255

	// maxFastSymlink is the (exclusive) bound for symlink targets stored in
	// the inode instead of a data block.
	maxFastSymlink = 60

	// defaultSpoolThreshold is how much of a streamed file is kept in memory
	// before it spills to a temporary file.
	defaultSpoolThreshold = 1 << 20

	// minLastGroupBlocks is the least number of blocks the final, partial
	// block group may have. Tiny trailing groups are padded to this size.
	minLastGroupBlocks = 64
)

// Options configures a [Builder].
type Options struct {
	// BlockSize is the filesystem block size in bytes: a power of two between
	// 1024 and 65536. The default is 4096. Linux can only mount block sizes
	// up to the page size of the machine.
	BlockSize int

	// Size is the minimum size of the image in bytes. The filesystem is
	// otherwise exactly as large as its content needs. When Size is larger,
	// the filesystem is extended to fill it and the extra space is free.
	Size int64

	// BytesPerInode, if positive, reserves one inode for every this many bytes
	// of [Options.Size], in addition to the inodes the content needs. With
	// zero, the filesystem has the inodes it needs plus ExtraInodes.
	BytesPerInode int64

	// ExtraInodes is the number of free inodes to leave in the filesystem
	// beyond those used by the content.
	ExtraInodes uint32

	// Time is used as the creation time of the filesystem and for every
	// inode that has no timestamp of its own. The default is the Unix epoch,
	// which keeps images reproducible.
	Time time.Time

	// UUID is the filesystem UUID. If zero, a UUID is derived from the
	// metadata of the content, so equal inputs produce equal images.
	UUID [16]byte

	// Label is the volume label (at most 16 bytes).
	Label string

	// OmitLostAndFound leaves out the lost+found directory that fsck uses
	// to reattach orphaned files. Without it, fsck reports the directory as
	// missing.
	OmitLostAndFound bool

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
	if o.BlockSize == 0 {
		o.BlockSize = defaultBlockSize
	}
	if o.BlockSize < minBlockSize || o.BlockSize > maxBlockSize || o.BlockSize&(o.BlockSize-1) != 0 {
		return fmt.Errorf("ext4: invalid block size %d: must be a power of two between %d and %d", o.BlockSize, minBlockSize, maxBlockSize)
	}
	if o.Size < 0 {
		return fmt.Errorf("ext4: negative image size %d", o.Size)
	}
	if o.BytesPerInode < 0 {
		return fmt.Errorf("ext4: negative bytes per inode %d", o.BytesPerInode)
	}
	if len(o.Label) > 16 {
		return fmt.Errorf("ext4: label %q is longer than 16 bytes", o.Label)
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
	// BlockSize is the filesystem block size in bytes.
	BlockSize int
	// Blocks is the number of blocks of the filesystem; UsedBlocks of them
	// are in use.
	Blocks, UsedBlocks uint64
	// Inodes is the number of inodes of the filesystem; UsedInodes of them
	// are in use (including the reserved ones).
	Inodes, UsedInodes uint32
	// Groups is the number of block groups.
	Groups int
	// UUID is the UUID of the filesystem.
	UUID [16]byte
	// Files lists the location of the data of every regular file with
	// content, in the order the data was laid out.
	Files []FileLayout
}

// FileLayout locates the content of a regular file inside the image. The
// content is stored contiguously: it is the Size bytes starting at Offset.
type FileLayout struct {
	// Path is the slash-separated path of the file, without leading slash.
	// For files with several hard links, it is the first path in sorted
	// order.
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
