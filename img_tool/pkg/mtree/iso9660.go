package mtree

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/iso9660"
)

// collectIso9660 walks an ISO 9660 image and captures every entry as if it had
// come from a tar, so the rest of the renderer needs no separate ISO 9660 path.
//
// Like for ext4, nothing is translated: an ISO 9660 image is the content of a
// filesystem, not a changeset, so whiteout markers are ordinary files. The
// metadata is as good as the image: with Rock Ridge it is the real names, modes
// and owners, and without it the mangled identifiers and the read-only modes
// ISO 9660 implies.
func collectIso9660(ra io.ReaderAt, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool, error), input int) ([]collectedEntry, error) {
	rd, err := iso9660.OpenReader(ra)
	if err != nil {
		return nil, fmt.Errorf("opening ISO 9660 image: %w", err)
	}

	var out []collectedEntry
	err = rd.Walk(func(e *iso9660.Entry) error {
		if e.Path == "" {
			return nil // the root directory is not an entry
		}
		hdr := iso9660Header(e)

		var digest []byte
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && digester != nil {
			digest, err = iso9660Digest(rd, hdr, e, digester, rangeDigester)
			if err != nil {
				return fmt.Errorf("digesting %q: %w", e.Path, err)
			}
		}
		out = append(out, collectedEntry{
			path:   cleanPath(e.Path),
			hdr:    hdr,
			digest: digest,
			nlink:  max(e.Nlink, 1),
			input:  input,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// iso9660Header builds the tar header describing an ISO 9660 entry.
func iso9660Header(e *iso9660.Entry) *tar.Header {
	hdr := &tar.Header{
		Name:    e.Path,
		Mode:    permBits(e.Mode),
		Uid:     e.Uid,
		Gid:     e.Gid,
		ModTime: e.Mtime,
		Format:  tar.FormatPAX,
	}
	switch {
	case e.Mode.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name = e.Path + "/"
	case e.Mode&fs.ModeSymlink != 0:
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = e.Target
	case e.Mode&fs.ModeDevice != 0:
		if e.Mode&fs.ModeCharDevice != 0 {
			hdr.Typeflag = tar.TypeChar
		} else {
			hdr.Typeflag = tar.TypeBlock
		}
		hdr.Devmajor, hdr.Devminor = int64(e.Major), int64(e.Minor)
	case e.Mode&fs.ModeNamedPipe != 0:
		hdr.Typeflag = tar.TypeFifo
	default:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = e.Size
	}
	return hdr
}

// iso9660Digest obtains a regular file's content digest. The writer stores a
// file as one contiguous extent, which is exactly the range a compact stream
// replaces with a content-addressed reference; a digester that knows the
// references (the compact-stream case) is asked first, so no payload has to be
// present. Otherwise the content is read out of the image and hashed.
func iso9660Digest(rd *iso9660.Reader, hdr *tar.Header, e *iso9660.Entry, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool, error)) ([]byte, error) {
	if rangeDigester != nil {
		digest, found, err := rangeDigester(e.Offset, e.Size)
		if err != nil {
			return nil, err
		}
		if found {
			return digest, nil
		}
	}
	return digester(hdr, rd.Content(e))
}
