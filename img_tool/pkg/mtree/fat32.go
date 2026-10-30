package mtree

import (
	"archive/tar"
	"fmt"
	"io"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32"
)

// collectFat32 walks a FAT32 image and captures every entry as if it had come
// from a tar, so the rest of the renderer needs no separate FAT32 path.
//
// Like for ext4, nothing is translated: a FAT32 image is the content of a file
// system, not a changeset, so whiteout markers are ordinary files. FAT stores
// neither ownership nor permissions, so entries are owned by root and carry the
// modes the reader reports.
func collectFat32(ra io.ReaderAt, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool, error), input int) ([]collectedEntry, error) {
	rd, err := fat32.OpenReader(ra)
	if err != nil {
		return nil, err
	}

	var out []collectedEntry
	err = rd.Walk(func(e *fat32.Entry) error {
		if e.Path == "" {
			return nil // the root directory is not an entry
		}
		hdr := fat32Header(e)

		var digest []byte
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && digester != nil {
			digest, err = fat32Digest(rd, hdr, e, digester, rangeDigester)
			if err != nil {
				return fmt.Errorf("digesting %q: %w", e.Path, err)
			}
		}
		out = append(out, collectedEntry{
			path:   cleanPath(e.Path),
			hdr:    hdr,
			digest: digest,
			nlink:  1,
			input:  input,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// fat32Header builds the tar header describing a FAT32 entry.
func fat32Header(e *fat32.Entry) *tar.Header {
	hdr := &tar.Header{
		Name:    e.Path,
		Mode:    permBits(e.Mode),
		ModTime: e.Mtime,
		Format:  tar.FormatPAX,
	}
	if e.Mode.IsDir() {
		hdr.Typeflag = tar.TypeDir
		hdr.Name = e.Path + "/"
		return hdr
	}
	hdr.Typeflag = tar.TypeReg
	hdr.Size = e.Size
	return hdr
}

// fat32Digest obtains a regular file's content digest. The writer stores a file
// as one contiguous run, which is exactly the range a compact stream replaces
// with a content-addressed reference; a digester that knows the references
// (the compact-stream case) is asked first, so no payload has to be present.
// Otherwise the content is read out of the image and hashed.
func fat32Digest(rd *fat32.Reader, hdr *tar.Header, e *fat32.Entry, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool, error)) ([]byte, error) {
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
