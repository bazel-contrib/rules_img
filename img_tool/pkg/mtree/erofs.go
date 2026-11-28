package mtree

import (
	"archive/tar"
	"fmt"
	"io/fs"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/overlayfs"
)

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
