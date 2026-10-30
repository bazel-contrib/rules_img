package mtree

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
)

// collectExt4 walks an ext4 filesystem image and captures every entry as if it
// had come from a tar, so the rest of the renderer needs no separate ext4 path.
//
// Unlike for EROFS, nothing is translated: an ext4 layer is a filesystem of its
// own, not a changeset, so whiteout markers are ordinary files.
func collectExt4(ra io.ReaderAt, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool, error), input int) ([]collectedEntry, error) {
	rd, err := ext4.OpenReader(ra)
	if err != nil {
		return nil, fmt.Errorf("opening ext4 image: %w", err)
	}

	var out []collectedEntry
	err = rd.Walk(func(p string, in *ext4.Inode) error {
		if p == "." {
			return nil // the root directory is not an entry
		}
		hdr := ext4Header(p, in)

		var digest []byte
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && digester != nil {
			var err error
			digest, err = ext4Digest(rd, hdr, in, digester, rangeDigester)
			if err != nil {
				return fmt.Errorf("digesting %q: %w", p, err)
			}
		}
		out = append(out, collectedEntry{
			path:   cleanPath(p),
			hdr:    hdr,
			digest: digest,
			nlink:  int(in.Links),
			input:  input,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ext4Header builds the tar header describing an ext4 inode.
func ext4Header(p string, in *ext4.Inode) *tar.Header {
	hdr := &tar.Header{
		Name:    p,
		Mode:    permBits(in.Mode),
		Uid:     int(in.UID),
		Gid:     int(in.GID),
		ModTime: in.Mtime,
		Format:  tar.FormatPAX,
	}
	switch {
	case in.Mode.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name = p + "/"
	case in.Mode&fs.ModeSymlink != 0:
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = in.Target
	case in.Mode&fs.ModeDevice != 0:
		if in.Mode&fs.ModeCharDevice != 0 {
			hdr.Typeflag = tar.TypeChar
		} else {
			hdr.Typeflag = tar.TypeBlock
		}
		hdr.Devmajor, hdr.Devminor = int64(in.Major), int64(in.Minor)
	case in.Mode&(fs.ModeNamedPipe|fs.ModeSocket) != 0:
		// A socket has no tar type; see collectErofs.
		hdr.Typeflag = tar.TypeFifo
	default:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = in.Size
	}
	if len(in.Xattrs) > 0 {
		hdr.PAXRecords = make(map[string]string, len(in.Xattrs))
		for k, v := range in.Xattrs {
			hdr.PAXRecords[schilyXattrPrefix+k] = v
		}
	}
	return hdr
}

// ext4Digest obtains a regular file's content digest. The writer stores a file
// as one contiguous run, which is exactly the range a compact stream replaces
// with a content-addressed reference; a digester that knows the references
// (the compact-stream case) is asked first, so no payload has to be present.
// Otherwise the content is read out of the image and hashed.
func ext4Digest(rd *ext4.Reader, hdr *tar.Header, in *ext4.Inode, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool, error)) ([]byte, error) {
	if rangeDigester != nil && len(in.Extents) > 0 {
		contiguous := true
		for i := 1; i < len(in.Extents); i++ {
			if in.Extents[i].Offset != in.Extents[i-1].Offset+in.Extents[i-1].Length {
				contiguous = false
			}
		}
		if contiguous {
			digest, found, err := rangeDigester(in.Extents[0].Offset, in.Size)
			if err != nil {
				return nil, err
			}
			if found {
				return digest, nil
			}
		}
	}
	return digester(hdr, rd.Content(in))
}
