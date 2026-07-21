package ztoc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// This file builds the TOC (table of contents) portion of a ztoc: one
// FileMetadata entry per tar entry, recording where each file's uncompressed
// bytes live in the decompressed stream. It mirrors soci-snapshotter's
// metadataFromTarReader and getType. Decompression here uses the standard
// library gzip reader (this pass does not need block boundaries), and the tar
// is parsed with archive/tar exactly as soci does, so the computed offsets and
// the total uncompressed size match soci byte-for-byte.

// tarBlockSize is the tar block size; tar entry data is padded to a multiple of
// it and the next header starts at the next boundary.
const tarBlockSize = 512

// countingReader is an io.Reader that tracks how many bytes have been read
// through it, mirroring soci's ioutils.PositionTrackerReader.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// buildTOC reads the gzip-compressed tar in data and returns the file metadata
// (in archive order) plus the total uncompressed archive size (the tar reader's
// final position, matching soci).
func buildTOC(data []byte) (TOC, Offset, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return TOC{}, 0, fmt.Errorf("ztoc: opening gzip stream: %w", err)
	}
	// Multistream is on by default; concatenated gzip members are read as one
	// continuous uncompressed stream.
	pt := &countingReader{r: gz}
	tr := tar.NewReader(pt)

	var md []FileMetadata
	for {
		hdr, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return TOC{}, 0, fmt.Errorf("ztoc: reading tar header: %w", err)
		}
		fileType, err := tarEntryType(hdr)
		if err != nil {
			return TOC{}, 0, err
		}
		md = append(md, FileMetadata{
			Name:               hdr.Name,
			Type:               fileType,
			UncompressedOffset: Offset(pt.n), // position after the header == start of data
			UncompressedSize:   Offset(hdr.Size),
			Linkname:           hdr.Linkname,
			Mode:               hdr.Mode,
			UID:                hdr.Uid,
			GID:                hdr.Gid,
			Uname:              hdr.Uname,
			Gname:              hdr.Gname,
			ModTime:            hdr.ModTime,
			Devmajor:           hdr.Devmajor,
			Devminor:           hdr.Devminor,
			PAXHeaders:         hdr.PAXRecords,
		})
	}
	return TOC{FileMetadata: md}, Offset(pt.n), nil
}

// tarEntryType maps a tar typeflag to the ztoc type string, matching soci.
func tarEntryType(hdr *tar.Header) (string, error) {
	switch hdr.Typeflag {
	case tar.TypeLink:
		return "hardlink", nil
	case tar.TypeSymlink:
		return "symlink", nil
	case tar.TypeDir:
		return "dir", nil
	case tar.TypeReg, tar.TypeRegA:
		return "reg", nil
	case tar.TypeChar:
		return "char", nil
	case tar.TypeBlock:
		return "block", nil
	case tar.TypeFifo:
		return "fifo", nil
	default:
		return "", fmt.Errorf("ztoc: unsupported tar entry type %q for %q", hdr.Typeflag, hdr.Name)
	}
}

// alignToTarBlock rounds an offset up to the next multiple of tarBlockSize.
func alignToTarBlock(o Offset) Offset {
	if r := o % tarBlockSize; r != 0 {
		o += tarBlockSize - r
	}
	return o
}
