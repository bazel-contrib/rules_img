package mtree

import (
	"archive/tar"
	"io/fs"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/iso9660"
)

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
