package mtree

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"path"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/overlayfs"
)

// collectErofs walks an EROFS filesystem image and captures every entry as if it
// had come from a tar, so the rest of the renderer -- keyword selection,
// hardlink counting, the changeset layout -- needs no separate EROFS path.
//
// Overlayfs whiteouts are translated back to tar-style marker entries so their
// meaning survives rendering a per-layer mtree and merging that spec later.
func collectErofs(ra io.ReaderAt, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool, error), input int) ([]collectedEntry, error) {
	efs, err := erofs.Open(ra)
	if err != nil {
		return nil, fmt.Errorf("opening EROFS image: %w", err)
	}

	var out []collectedEntry
	err = fs.WalkDir(efs, ".", func(p string, d fs.DirEntry, err error) error {
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
			marker := path.Join(p, whiteoutOpaque)
			out = append(out, collectedEntry{
				path: marker,
				hdr: &tar.Header{
					Name:     marker,
					Typeflag: tar.TypeReg,
					Mode:     0o644,
					Format:   tar.FormatPAX,
				},
				nlink: 1,
				input: input,
			})
			delete(hdr.PAXRecords, schilyXattrPrefix+overlayfs.OpaqueXattr)
		}
		if p == "." {
			// Keep the root's opacity marker, but omit its ordinary metadata.
			return nil
		}
		nlink := st.Nlink
		if isOverlayWhiteout(hdr) {
			p = path.Join(path.Dir(p), whiteoutPrefix+path.Base(p))
			hdr.Name = p
			hdr.Typeflag = tar.TypeReg
			nlink = 1
		}

		var digest []byte
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && digester != nil {
			digest, err = erofsDigest(efs, p, hdr, info, digester, rangeDigester)
			if err != nil {
				return fmt.Errorf("digesting %q: %w", p, err)
			}
		}

		out = append(out, collectedEntry{
			path:   cleanPath(p),
			hdr:    hdr,
			digest: digest,
			nlink:  nlink,
			input:  input,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// erofsHeader builds the tar header describing an EROFS inode.
func erofsHeader(efs fs.FS, p string, info fs.FileInfo, st *erofs.Stat) (*tar.Header, error) {
	hdr := &tar.Header{
		Name:    p,
		Mode:    int64(permBits(st.Mode)),
		Uid:     int(st.UID),
		Gid:     int(st.GID),
		ModTime: info.ModTime(),
		Format:  tar.FormatPAX,
	}
	switch {
	case st.Mode.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name = p + "/"
	case st.Mode&fs.ModeSymlink != 0:
		hdr.Typeflag = tar.TypeSymlink
		target, err := readLink(efs, p)
		if err != nil {
			return nil, err
		}
		hdr.Linkname = target
	case st.Mode&fs.ModeDevice != 0:
		if st.Mode&fs.ModeCharDevice != 0 {
			hdr.Typeflag = tar.TypeChar
		} else {
			hdr.Typeflag = tar.TypeBlock
		}
		hdr.Devmajor, hdr.Devminor = overlayfs.DecodeDev(st.Rdev)
	case st.Mode&fs.ModeNamedPipe != 0:
		hdr.Typeflag = tar.TypeFifo
	case st.Mode&fs.ModeSocket != 0:
		// A socket has no tar type; render it as a FIFO, which is the closest
		// thing the mtree type set has for a non-file, non-device special.
		hdr.Typeflag = tar.TypeFifo
	default:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = st.Size
	}
	if len(st.Xattrs) > 0 {
		hdr.PAXRecords = make(map[string]string, len(st.Xattrs))
		for k, v := range st.Xattrs {
			hdr.PAXRecords[schilyXattrPrefix+k] = v
		}
	}
	return hdr, nil
}

// erofsDigest obtains a regular file's content digest. A digester that can
// answer from the layer's recorded references (the compact-stream case) is asked
// first with the file's byte range in the image, so no payload has to be
// present; otherwise the payload is read out of the image and hashed.
func erofsDigest(efs fs.FS, p string, hdr *tar.Header, info fs.FileInfo, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool, error)) ([]byte, error) {
	if rangeDigester != nil {
		if ranger, ok := info.(interface{ DataRange() []erofs.DataRange }); ok {
			ranges := ranger.DataRange()
			for _, r := range ranges {
				if r.Offset < 0 || r.Device != 0 {
					continue
				}
				// A payload stored as one contiguous extent is exactly the range a
				// compact stream replaced with a content-addressed reference. Any
				// other overlap with a reference is an error from the digester:
				// the image reads as zeros there, and hashing it would be wrong.
				digest, found, err := rangeDigester(r.Offset, r.Size)
				if err != nil {
					return nil, err
				}
				if !found {
					continue
				}
				if len(ranges) != 1 {
					return nil, fmt.Errorf("only part of the payload is a content-addressed reference, so its digest is unknown")
				}
				return digest, nil
			}
		}
	}
	f, err := efs.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return digester(hdr, f)
}

// readLink reads a symlink target from an fs.FS that supports it.
func readLink(efs fs.FS, p string) (string, error) {
	rl, ok := efs.(interface {
		ReadLink(string) (string, error)
	})
	if !ok {
		return "", fmt.Errorf("%s: EROFS image does not support reading symlink targets", p)
	}
	return rl.ReadLink(p)
}

// permBits converts an fs.FileMode back to the permission bits a tar header
// stores, including setuid, setgid and sticky.
func permBits(mode fs.FileMode) int64 {
	perm := int64(mode.Perm())
	if mode&fs.ModeSetuid != 0 {
		perm |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		perm |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		perm |= 0o1000
	}
	return perm
}

// isOverlayWhiteout reports whether an EROFS-origin entry is an overlayfs
// whiteout: a character device with device number 0 in place of the deleted
// entry.
func isOverlayWhiteout(hdr *tar.Header) bool {
	return hdr != nil && hdr.Typeflag == tar.TypeChar && hdr.Devmajor == 0 && hdr.Devminor == 0
}

// isOverlayOpaque reports whether an entry carries the overlayfs opaque marker.
func isOverlayOpaque(hdr *tar.Header) bool {
	return hdr != nil && hdr.PAXRecords[schilyXattrPrefix+overlayfs.OpaqueXattr] == "y"
}
