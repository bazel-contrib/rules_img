package mtree

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio"
)

// collectCpio walks a cpio archive and captures every entry as if it had come
// from a tar, so the rest of the renderer needs no separate cpio path.
//
// Like for ext4, nothing is translated: a cpio archive is the content of a file
// system, not a changeset, so whiteout markers are ordinary files.
func collectCpio(ra io.ReaderAt, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool, error), input int) ([]collectedEntry, error) {
	rd := cpio.OpenReader(ra)

	// The members of a set of hard links share one copy of the content.
	type span struct{ offset, size int64 }
	digests := map[span][]byte{}

	var out []collectedEntry
	err := rd.Walk(func(e *cpio.Entry) error {
		if e.Name == "" {
			return nil // the root directory is not an entry
		}
		hdr := cpioHeader(e)

		var digest []byte
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && digester != nil {
			key := span{e.Offset, e.Size}
			digest = digests[key]
			if digest == nil {
				var err error
				digest, err = cpioDigest(rd, hdr, e, digester, rangeDigester)
				if err != nil {
					return fmt.Errorf("digesting %q: %w", e.Name, err)
				}
				digests[key] = digest
			}
		}
		out = append(out, collectedEntry{
			path:   cleanPath(e.Name),
			hdr:    hdr,
			digest: digest,
			nlink:  int(e.Nlink),
			input:  input,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// cpioHeader builds the tar header describing a cpio member.
func cpioHeader(e *cpio.Entry) *tar.Header {
	hdr := &tar.Header{
		Name:    e.Name,
		Mode:    permBits(e.Mode),
		Uid:     int(e.UID),
		Gid:     int(e.GID),
		ModTime: e.Mtime,
		Format:  tar.FormatPAX,
	}
	switch {
	case e.Mode.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name = e.Name + "/"
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
	case e.Mode&(fs.ModeNamedPipe|fs.ModeSocket) != 0:
		// A socket has no tar type; see collectErofs.
		hdr.Typeflag = tar.TypeFifo
	default:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = e.Size
	}
	return hdr
}

// cpioDigest obtains a regular file's content digest. The writer stores a file
// as one contiguous run, which is exactly the range a compact stream replaces
// with a content-addressed reference; a digester that knows the references
// (the compact-stream case) is asked first, so no payload has to be present.
// Otherwise the content is read out of the archive and hashed.
func cpioDigest(rd *cpio.Reader, hdr *tar.Header, e *cpio.Entry, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool, error)) ([]byte, error) {
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
