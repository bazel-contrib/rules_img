package mtree

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
)

// OverlayOpaqueXattr marks a directory in an EROFS layer as opaque: it hides
// the same directory's entries in lower layers. It is the overlayfs-native
// equivalent of a ".wh..wh..opq" marker, and the changeset layout applies it the
// same way.
const OverlayOpaqueXattr = "trusted.overlay.opaque"

// collectErofs walks an EROFS filesystem image and captures every entry as if it
// had come from a tar, so the rest of the renderer -- keyword selection,
// hardlink counting, the changeset layout -- needs no separate EROFS path.
//
// Deliberate differences from the same layer rendered as a tar, which the
// docs and the equivalence test both spell out: an EROFS image carries every
// intermediate directory (a filesystem needs a complete tree, a changeset does
// not), and it expresses deletions as overlayfs whiteouts rather than ".wh."
// marker files.
func collectErofs(ra io.ReaderAt, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool), input int) ([]collectedEntry, error) {
	efs, err := erofs.Open(ra)
	if err != nil {
		return nil, fmt.Errorf("opening EROFS image: %w", err)
	}

	var out []collectedEntry
	err = fs.WalkDir(efs, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			// The image root is not an entry of the layer; a tar layer does not
			// describe it either.
			return nil
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

		var digest []byte
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && digester != nil {
			digest, err = erofsDigest(efs, p, hdr, info, digester, rangeDigester)
			if err != nil {
				return fmt.Errorf("digesting %q: %w", p, err)
			}
		}

		out = append(out, collectedEntry{
			path:    cleanPath(p),
			hdr:     hdr,
			digest:  digest,
			nlink:   st.Nlink,
			overlay: true,
			input:   input,
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
		hdr.Devmajor, hdr.Devminor = decodeDev(st.Rdev)
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
func erofsDigest(efs fs.FS, p string, hdr *tar.Header, info fs.FileInfo, digester ContentDigester, rangeDigester func(offset, size int64) ([]byte, bool)) ([]byte, error) {
	if rangeDigester != nil {
		if ranger, ok := info.(interface{ DataRange() []erofs.DataRange }); ok {
			// A payload stored as one contiguous extent is exactly the range a
			// compact stream replaced with a content-addressed reference.
			if r := ranger.DataRange(); len(r) == 1 && r[0].Offset >= 0 && r[0].Device == 0 {
				if digest, ok := rangeDigester(r[0].Offset, r[0].Size); ok {
					return digest, nil
				}
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

// decodeDev unpacks a device number stored the way the kernel's new_encode_dev
// packs it, which is how EROFS stores i_rdev.
func decodeDev(rdev uint32) (major, minor int64) {
	major = int64((rdev & 0xfff00) >> 8)
	minor = int64(rdev&0xff) | int64((rdev>>12)&0xfff00)
	return major, minor
}

// isOverlayWhiteout reports whether an EROFS-origin entry is an overlayfs
// whiteout: a character device with device number 0 in place of the deleted
// entry.
func isOverlayWhiteout(hdr *tar.Header) bool {
	return hdr != nil && hdr.Typeflag == tar.TypeChar && hdr.Devmajor == 0 && hdr.Devminor == 0
}

// isOverlayOpaque reports whether an entry carries the overlayfs opaque marker.
func isOverlayOpaque(hdr *tar.Header) bool {
	return hdr != nil && hdr.PAXRecords[schilyXattrPrefix+OverlayOpaqueXattr] == "y"
}
