package ext4

import (
	"encoding/binary"
	"hash/crc32"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// crc32c continues a crc32c the way the Linux kernel's crc32c_le does: the
// seed is the raw register value, with no inversion on input or output.
// (hash/crc32 inverts both, so the seed and the result are inverted here.)
func crc32c(seed uint32, p []byte) uint32 {
	return ^crc32.Update(^seed, castagnoli, p)
}

// crc32cLE32 continues a crc32c over the little endian encoding of v.
func crc32cLE32(seed, v uint32) uint32 {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return crc32c(seed, b[:])
}
