package mtree

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"path"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/iso9660"
	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/overlayfs"
)

// rangeDigesterFunc reports a digest already recorded for a byte range of a
// filesystem image (see Input.RangeDigester).
type rangeDigesterFunc = func(offset, size int64) ([]byte, bool, error)

// fsEntry is one entry of a filesystem image layer (EROFS, ext4, FAT32, ISO
// 9660 or cpio), rendered as a tar header.
type fsEntry struct {
	hdr   *tar.Header
	nlink int
	// open returns the payload of a regular file. It is nil for other entries.
	open func() (io.Reader, error)
	// recorded asks a rangeDigester for the digest a compact stream recorded for
	// the payload's byte range, so no payload has to be present. nil if the
	// format has no such shortcut for this entry.
	recorded func(rangeDigester rangeDigesterFunc) ([]byte, bool, error)
	// shared identifies a payload that several entries share (the members of a
	// set of cpio hard links), or is nil.
	shared any
}

// WalkFilesystem calls fn for every entry of a filesystem image layer, in the
// order the image stores them, as a tar header. For a regular file, content
// reads its payload; it is only valid during the call, and is nil for every
// other entry.
//
// kind is one of ErofsInput, Ext4Input, Fat32Input, Iso9660Input or CpioInput.
// The entries are what an mtree of the layer describes: the root directory is
// left out, and an EROFS layer's overlayfs whiteouts become tar-style marker
// entries (".wh.<name>" and ".wh..wh..opq"), so the result reads like a tar
// changeset.
func WalkFilesystem(kind InputKind, ra io.ReaderAt, fn func(hdr *tar.Header, content io.Reader) error) error {
	return walkFilesystem(kind, ra, func(e fsEntry) error {
		if e.open == nil || e.hdr.Size == 0 {
			return fn(e.hdr, nil)
		}
		content, err := e.open()
		if err != nil {
			return fmt.Errorf("opening %q: %w", e.hdr.Name, err)
		}
		if closer, ok := content.(io.Closer); ok {
			defer closer.Close()
		}
		return fn(e.hdr, io.LimitReader(content, e.hdr.Size))
	})
}

func walkFilesystem(kind InputKind, ra io.ReaderAt, fn func(fsEntry) error) error {
	switch kind {
	case ErofsInput:
		return walkErofs(ra, fn)
	case Ext4Input:
		return walkExt4(ra, fn)
	case Fat32Input:
		return walkFat32(ra, fn)
	case Iso9660Input:
		return walkIso9660(ra, fn)
	case CpioInput:
		return walkCpio(ra, fn)
	default:
		return fmt.Errorf("input kind %d is not a filesystem image", kind)
	}
}

// collectFilesystem walks a filesystem image and captures every entry as if it
// had come from a tar, so the rest of the renderer -- keyword selection,
// hardlink counting, the changeset layout -- needs no separate path per format.
func collectFilesystem(kind InputKind, ra io.ReaderAt, digester ContentDigester, rangeDigester rangeDigesterFunc, input int) ([]collectedEntry, error) {
	shared := map[any][]byte{}
	var out []collectedEntry
	err := walkFilesystem(kind, ra, func(e fsEntry) error {
		var digest []byte
		if e.hdr.Typeflag == tar.TypeReg && e.hdr.Size > 0 && digester != nil {
			if e.shared != nil {
				digest = shared[e.shared]
			}
			if digest == nil {
				var err error
				digest, err = payloadDigest(e, digester, rangeDigester)
				if err != nil {
					return fmt.Errorf("digesting %q: %w", e.hdr.Name, err)
				}
				if e.shared != nil {
					shared[e.shared] = digest
				}
			}
		}
		out = append(out, collectedEntry{
			path:   cleanPath(e.hdr.Name),
			hdr:    e.hdr,
			digest: digest,
			nlink:  e.nlink,
			input:  input,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// payloadDigest obtains a regular file's content digest. A digester that can
// answer from the layer's recorded references (the compact-stream case) is asked
// first, so no payload has to be present; otherwise the payload is read out of
// the image and hashed.
func payloadDigest(e fsEntry, digester ContentDigester, rangeDigester rangeDigesterFunc) ([]byte, error) {
	if rangeDigester != nil && e.recorded != nil {
		digest, found, err := e.recorded(rangeDigester)
		if err != nil {
			return nil, err
		}
		if found {
			return digest, nil
		}
	}
	content, err := e.open()
	if err != nil {
		return nil, err
	}
	if closer, ok := content.(io.Closer); ok {
		defer closer.Close()
	}
	return digester(e.hdr, content)
}

// recordedRange is the recorded shortcut of a payload stored as one contiguous
// run, which is exactly the range a compact stream replaced with a
// content-addressed reference.
func recordedRange(offset, size int64) func(rangeDigesterFunc) ([]byte, bool, error) {
	return func(rangeDigester rangeDigesterFunc) ([]byte, bool, error) {
		return rangeDigester(offset, size)
	}
}

// walkErofs walks an EROFS filesystem image. Overlayfs whiteouts are translated
// back to tar-style marker entries so their meaning survives rendering a
// per-layer mtree and merging that spec later.
func walkErofs(ra io.ReaderAt, fn func(fsEntry) error) error {
	efs, err := erofs.Open(ra)
	if err != nil {
		return fmt.Errorf("opening EROFS image: %w", err)
	}

	return fs.WalkDir(efs, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		st, ok := info.Sys().(*erofs.Stat)
		if !ok {
			return fmt.Errorf("%s: EROFS image did not report inode metadata", p)
		}

		hdr, err := erofsHeader(efs, p, info, st)
		if err != nil {
			return err
		}

		if isOverlayOpaque(hdr) {
			marker := path.Join(p, WhiteoutOpaque)
			if err := fn(fsEntry{
				hdr: &tar.Header{
					Name:     marker,
					Typeflag: tar.TypeReg,
					Mode:     0o644,
					Format:   tar.FormatPAX,
				},
				nlink: 1,
			}); err != nil {
				return err
			}
			delete(hdr.PAXRecords, schilyXattrPrefix+overlayfs.OpaqueXattr)
		}
		if p == "." {
			// Keep the root's opacity marker, but omit its ordinary metadata.
			return nil
		}
		entry := fsEntry{hdr: hdr, nlink: int(st.Nlink)}
		if isOverlayWhiteout(hdr) {
			p = path.Join(path.Dir(p), WhiteoutPrefix+path.Base(p))
			hdr.Name = p
			hdr.Typeflag = tar.TypeReg
			entry.nlink = 1
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			name := p
			entry.open = func() (io.Reader, error) { return efs.Open(name) }
			entry.recorded = erofsRecorded(info)
		}
		return fn(entry)
	})
}

// erofsRecorded is the recorded shortcut of an EROFS payload: a payload stored
// as one contiguous extent is exactly the range a compact stream replaced with a
// content-addressed reference. Any other overlap with a reference is an error:
// the image reads as zeros there, and hashing it would be wrong.
func erofsRecorded(info fs.FileInfo) func(rangeDigesterFunc) ([]byte, bool, error) {
	ranger, ok := info.(interface{ DataRange() []erofs.DataRange })
	if !ok {
		return nil
	}
	return func(rangeDigester rangeDigesterFunc) ([]byte, bool, error) {
		ranges := ranger.DataRange()
		for _, r := range ranges {
			if r.Offset < 0 || r.Device != 0 {
				continue
			}
			digest, found, err := rangeDigester(r.Offset, r.Size)
			if err != nil {
				return nil, false, err
			}
			if !found {
				continue
			}
			if len(ranges) != 1 {
				return nil, false, fmt.Errorf("only part of the payload is a content-addressed reference, so its digest is unknown")
			}
			return digest, true, nil
		}
		return nil, false, nil
	}
}

// walkExt4 walks an ext4 filesystem image. Unlike for EROFS, nothing is
// translated: an ext4 layer is a filesystem of its own, not a changeset, so
// whiteout markers are ordinary files.
func walkExt4(ra io.ReaderAt, fn func(fsEntry) error) error {
	rd, err := ext4.OpenReader(ra)
	if err != nil {
		return fmt.Errorf("opening ext4 image: %w", err)
	}
	return rd.Walk(func(p string, in *ext4.Inode) error {
		if p == "." {
			return nil // the root directory is not an entry
		}
		entry := fsEntry{hdr: ext4Header(p, in), nlink: int(in.Links)}
		if entry.hdr.Typeflag == tar.TypeReg && entry.hdr.Size > 0 {
			entry.open = func() (io.Reader, error) { return rd.Content(in), nil }
			// The writer stores a file as one contiguous run.
			if len(in.Extents) > 0 && ext4Contiguous(in.Extents) {
				entry.recorded = recordedRange(in.Extents[0].Offset, in.Size)
			}
		}
		return fn(entry)
	})
}

func ext4Contiguous(extents []ext4.Extent) bool {
	for i := 1; i < len(extents); i++ {
		if extents[i].Offset != extents[i-1].Offset+extents[i-1].Length {
			return false
		}
	}
	return true
}

// walkFat32 walks a FAT32 image. Like for ext4, nothing is translated. FAT
// stores neither ownership nor permissions, so entries are owned by root and
// carry the modes the reader reports.
func walkFat32(ra io.ReaderAt, fn func(fsEntry) error) error {
	rd, err := fat32.OpenReader(ra)
	if err != nil {
		return err
	}
	return rd.Walk(func(e *fat32.Entry) error {
		if e.Path == "" {
			return nil // the root directory is not an entry
		}
		entry := fsEntry{hdr: fat32Header(e), nlink: 1}
		if entry.hdr.Typeflag == tar.TypeReg && entry.hdr.Size > 0 {
			entry.open = func() (io.Reader, error) { return rd.Content(e), nil }
			entry.recorded = recordedRange(e.Offset, e.Size)
		}
		return fn(entry)
	})
}

// walkIso9660 walks an ISO 9660 image. Like for ext4, nothing is translated. The
// metadata is as good as the image: with Rock Ridge it is the real names, modes
// and owners, and without it the mangled identifiers and the read-only modes
// ISO 9660 implies.
func walkIso9660(ra io.ReaderAt, fn func(fsEntry) error) error {
	rd, err := iso9660.OpenReader(ra)
	if err != nil {
		return fmt.Errorf("opening ISO 9660 image: %w", err)
	}
	return rd.Walk(func(e *iso9660.Entry) error {
		if e.Path == "" {
			return nil // the root directory is not an entry
		}
		entry := fsEntry{hdr: iso9660Header(e), nlink: max(e.Nlink, 1)}
		if entry.hdr.Typeflag == tar.TypeReg && entry.hdr.Size > 0 {
			entry.open = func() (io.Reader, error) { return rd.Content(e), nil }
			entry.recorded = recordedRange(e.Offset, e.Size)
		}
		return fn(entry)
	})
}

// walkCpio walks a cpio archive. Like for ext4, nothing is translated: a cpio
// archive is the content of a file system, not a changeset.
func walkCpio(ra io.ReaderAt, fn func(fsEntry) error) error {
	// The members of a set of hard links share one copy of the content.
	type span struct{ offset, size int64 }

	rd := cpio.OpenReader(ra)
	return rd.Walk(func(e *cpio.Entry) error {
		if e.Name == "" {
			return nil // the root directory is not an entry
		}
		entry := fsEntry{hdr: cpioHeader(e), nlink: int(e.Nlink)}
		if entry.hdr.Typeflag == tar.TypeReg && entry.hdr.Size > 0 {
			entry.open = func() (io.Reader, error) { return rd.Content(e), nil }
			entry.recorded = recordedRange(e.Offset, e.Size)
			entry.shared = span{e.Offset, e.Size}
		}
		return fn(entry)
	})
}
