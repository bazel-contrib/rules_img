package mtree

import (
	"archive/tar"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32"
)

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
