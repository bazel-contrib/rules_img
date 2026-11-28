package mtree

import (
	"archive/tar"
	"io/fs"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/cpio"
)

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
