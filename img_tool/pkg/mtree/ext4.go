package mtree

import (
	"archive/tar"
	"io/fs"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
)

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
