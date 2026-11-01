package diskimage

import (
	"encoding/binary"
	"fmt"
)

// fsKind is the kind of file system found in a partition image.
type fsKind string

const (
	fsUnknown fsKind = "unknown"
	fsFAT32   fsKind = "fat32"
	fsExt4    fsKind = "ext4"
	fsErofs   fsKind = "erofs"
)

// sniffLen is how much of the start of an image is needed to tell its kind.
const sniffLen = 4096

// sniff tells the kind of file system from the first bytes of an image.
func sniff(head []byte) fsKind {
	le := binary.LittleEndian
	switch {
	case len(head) >= 512 && head[510] == 0x55 && head[511] == 0xAA && string(head[82:90]) == "FAT32   ":
		return fsFAT32
	case len(head) >= 1024+0x3A && le.Uint16(head[1024+0x38:]) == 0xEF53:
		return fsExt4
	case len(head) >= 1028 && le.Uint32(head[1024:]) == 0xE0F5E1E2:
		return fsErofs
	}
	return fsUnknown
}

// fat32Geometry reads the sector size and the position of the backup boot
// sector from the BPB of a FAT32 image.
func fat32Geometry(head []byte) (bytesPerSector, backupSector int) {
	return int(binary.LittleEndian.Uint16(head[11:])), int(binary.LittleEndian.Uint16(head[50:]))
}

// hiddenSectorsOffset is where the BPB keeps the number of hidden sectors.
const hiddenSectorsOffset = 28

// hiddenSectorPatches returns the patches that set the hidden sectors of a
// FAT32 file system, in its boot sector and in the backup boot sector, to the
// first sector of the partition. Firmware and boot loaders use it to find the
// file system from the start of the disk.
func hiddenSectorPatches(head []byte, size int64, startLBA uint64) ([]patchSpec, error) {
	if startLBA > 0xFFFFFFFF {
		return nil, fmt.Errorf("the partition starts at sector %d, which does not fit the 32 bit hidden sectors field of FAT32", startLBA)
	}
	var value [4]byte
	binary.LittleEndian.PutUint32(value[:], uint32(startLBA))
	patches := []patchSpec{{offset: hiddenSectorsOffset, data: value[:]}}
	bps, backup := fat32Geometry(head)
	if backup != 0 {
		off := int64(backup)*int64(bps) + hiddenSectorsOffset
		if off+4 > size {
			return nil, fmt.Errorf("the backup boot sector (sector %d) is beyond the end of the file system", backup)
		}
		patches = append(patches, patchSpec{offset: off, data: value[:]})
	}
	return patches, nil
}

type patchSpec struct {
	offset int64
	data   []byte
}
