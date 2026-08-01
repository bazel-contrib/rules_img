// Package overlayfs holds the overlayfs-native encodings an EROFS layer uses,
// shared by the writer (erofscas) and the reader (mtree) so the two cannot
// drift apart.
package overlayfs

// OpaqueXattr marks a directory in an EROFS layer as opaque: it hides the same
// directory's entries in lower layers. It is the overlayfs-native equivalent of
// a ".wh..wh..opq" marker file.
const OpaqueXattr = "trusted.overlay.opaque"

// EncodeDev packs a device's major and minor numbers the way the kernel's
// new_encode_dev does, which is how EROFS stores i_rdev.
func EncodeDev(major, minor int64) uint32 {
	return uint32(major&0xfff)<<8 | uint32(minor&0xff) | uint32(minor&^0xff)<<12
}

// DecodeDev is the inverse of [EncodeDev].
func DecodeDev(rdev uint32) (major, minor int64) {
	major = int64((rdev & 0xfff00) >> 8)
	minor = int64(rdev&0xff) | int64((rdev>>12)&0xfff00)
	return major, minor
}
