package erofs

import (
	"fmt"
	"io"
)

// ExtentKind classifies the payload a [Extent] covers.
type ExtentKind int

const (
	// ExtentFileData is a regular file's payload, stored verbatim.
	ExtentFileData ExtentKind = iota
	// ExtentDirents is an out-of-line directory block (or run of blocks).
	ExtentDirents
	// ExtentSymlink is an out-of-line symlink target.
	ExtentSymlink
)

func (k ExtentKind) String() string {
	switch k {
	case ExtentFileData:
		return "file-data"
	case ExtentDirents:
		return "dirents"
	case ExtentSymlink:
		return "symlink"
	default:
		return fmt.Sprintf("ExtentKind(%d)", int(k))
	}
}

// Extent is one out-of-line payload in the image's data area.
//
// Offset is absolute within the image and block-aligned. Size is the payload
// length; the Pad bytes that follow it are zero. Consecutive extents, together
// with their padding, tile [Layout.MetaSize, Layout.ImageSize) exactly.
type Extent struct {
	// Path is the image path of the inode that owns the payload.
	Path string
	// Kind is the payload's classification.
	Kind ExtentKind
	// Offset is the payload's absolute byte offset in the image.
	Offset int64
	// Size is the payload length in bytes, excluding trailing padding.
	Size int64
	// Pad is the number of zero bytes following the payload, up to the next
	// block boundary.
	Pad int64
	// Token is the opaque value the caller attached to the owning inode with
	// [Writer.SetToken], or nil.
	Token any
}

// Layout is the on-disk layout [Writer.WriteTo] will produce.
//
// The layout is metadata-first: [0, MetaSize) holds the superblock area and the
// inode metadata (including every inline payload), and [MetaSize, ImageSize) is
// tiled by Extents and their padding.
type Layout struct {
	// BlockSize is the image's filesystem block size in bytes.
	BlockSize int
	// ImageSize is the total number of bytes WriteTo will emit.
	ImageSize int64
	// MetaSize is the size of the superblock plus metadata area; the data area
	// starts here.
	MetaSize int64
	// Extents are the out-of-line payloads, ascending by Offset and
	// non-overlapping.
	Extents []Extent
}

// Prepare finalizes the tree and computes the on-disk layout without writing
// anything.
//
// It is idempotent: repeated calls return the same *Layout, and [Writer.WriteTo]
// produces exactly the layout Prepare returned. Prepare uses the metadata-first
// layout, so WriteTo needs no seeking. After Prepare the tree must not be
// modified; entry-adding methods return an error.
func (fsys *Writer) Prepare() (*Layout, error) {
	if fsys.wErr != nil {
		return nil, fsys.wErr
	}
	if fsys.layout != nil {
		return fsys.layout, nil
	}
	if fsys.closed {
		return nil, fmt.Errorf("mkfs: FS already closed")
	}

	ew, err := fsys.finalizeTree()
	if err != nil {
		return nil, err
	}

	// Metadata-first: the metadata area starts immediately after the
	// superblock area, and data blocks follow it.
	ew.metaBlkAddr = uint32(ew.sbAreaBlocks())
	ew.assignDataBlocks()
	if err := ew.resolveSharedData(); err != nil {
		return nil, err
	}

	metaBytes := ew.metadataBytes()
	metaBlocks := (metaBytes + ew.blockSize - 1) / ew.blockSize
	dataBlocks := 0
	for _, e := range ew.entries {
		if ds := ew.flatPlainDataSize(e); ds > 0 {
			dataBlocks += (ds + ew.blockSize - 1) / ew.blockSize
		}
	}

	fsys.ew = ew
	fsys.layout = &Layout{
		BlockSize: ew.blockSize,
		MetaSize:  int64(ew.sbAreaBlocks()+metaBlocks) * int64(ew.blockSize),
		ImageSize: int64(ew.sbAreaBlocks()+metaBlocks+dataBlocks) * int64(ew.blockSize),
		Extents:   ew.buildExtents(),
	}
	return fsys.layout, nil
}

// WriteTo serializes the image to w in ascending offset order using the
// metadata-first layout: superblock area, metadata, then data blocks. It
// requires no seeking and emits exactly the layout [Writer.Prepare] returned.
//
// WriteTo calls Prepare if the caller has not. The Writer must not be used
// afterwards.
func (fsys *Writer) WriteTo(w io.Writer) (int64, error) {
	layout, err := fsys.Prepare()
	if err != nil {
		return 0, err
	}
	if fsys.closed {
		return 0, fmt.Errorf("mkfs: FS already closed")
	}
	fsys.closed = true
	if fsys.spool != nil {
		defer func() { _ = fsys.spool.Close() }()
	}

	ew := fsys.ew
	ew.copyBuf = make([]byte, 256*1024)
	out := &countingWriter{w: w}

	if err := ew.writeBlock0(out); err != nil {
		return out.n, err
	}
	meta := ew.newMetaBuffer()
	if err := ew.writeMetadataInodes(meta); err != nil {
		return out.n, err
	}
	if _, err := meta.WriteTo(out); err != nil {
		return out.n, err
	}
	if err := ew.writeDataBlocks(out); err != nil {
		return out.n, err
	}

	if out.n != layout.ImageSize {
		return out.n, fmt.Errorf("mkfs: wrote %d bytes, layout planned %d", out.n, layout.ImageSize)
	}
	return out.n, nil
}

// countingWriter counts the bytes written through it. WriteTo uses it to check
// the emitted image against the planned layout.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// buildExtents materializes the out-of-line payload list in the same order
// writeDataBlocks emits it, which is also ascending by offset because
// assignDataBlocks allocates in entry order.
func (w *erofsWriter) buildExtents() []Extent {
	var extents []Extent
	for _, e := range w.entries {
		ds := w.flatPlainDataSize(e)
		if ds == 0 {
			continue
		}
		var pad int64
		if r := ds % w.blockSize; r != 0 {
			pad = int64(w.blockSize - r)
		}
		extents = append(extents, Extent{
			Path:   e.path,
			Kind:   extentKind(e),
			Offset: int64(e.dataBlkAddr) * int64(w.blockSize),
			Size:   int64(ds),
			Pad:    pad,
			Token:  e.token,
		})
	}
	return extents
}
