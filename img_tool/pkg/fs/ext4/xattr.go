package ext4

import (
	"archive/tar"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

const (
	xattrMagic       = 0xEA020000
	xattrBlockHeader = 32
	xattrEntryHeader = 16

	// The in-inode attribute area follows the 4 byte magic at the end of the
	// extra inode fields.
	ibodyOffset = 128 + extraIsize
	ibodySize   = inodeSize - ibodyOffset

	paxXattrPrefix = "SCHILY.xattr."
)

// xattr is an extended attribute. name has the prefix replaced by the
// ext4 name index.
type xattr struct {
	index uint8
	name  string
	value []byte
}

// Name indices, by prefix. The longest matching prefix wins, so the posix
// ACL names are listed before the generic system prefix.
var xattrPrefixes = []struct {
	index  uint8
	prefix string
}{
	{2, "system.posix_acl_access"},
	{3, "system.posix_acl_default"},
	{1, "user."},
	{4, "trusted."},
	{6, "security."},
	{7, "system."},
}

func xattrIndex(full string) (uint8, string, error) {
	for _, p := range xattrPrefixes {
		if strings.HasPrefix(full, p.prefix) {
			short := full[len(p.prefix):]
			if len(short) > 255 {
				return 0, "", fmt.Errorf("extended attribute name %q is too long", full)
			}
			return p.index, short, nil
		}
	}
	return 0, "", fmt.Errorf("extended attribute %q has an unsupported namespace", full)
}

// xattrsFromHeader extracts the extended attributes a tar header carries as
// PAX "SCHILY.xattr." records. They are sorted the way ext4 orders them.
func xattrsFromHeader(hdr *tar.Header) ([]xattr, error) {
	values := map[string]string{}
	for name, value := range hdr.Xattrs { //nolint:staticcheck // still filled by older producers
		values[name] = value
	}
	for key, value := range hdr.PAXRecords {
		if name, ok := strings.CutPrefix(key, paxXattrPrefix); ok {
			values[name] = value
		}
	}
	if len(values) == 0 {
		return nil, nil
	}

	xs := make([]xattr, 0, len(values))
	for full, value := range values {
		index, short, err := xattrIndex(full)
		if err != nil {
			return nil, err
		}
		data := []byte(value)
		if index == 2 || index == 3 {
			if data, err = posixACLToDisk(data); err != nil {
				return nil, fmt.Errorf("%s: %w", full, err)
			}
		}
		xs = append(xs, xattr{index: index, name: short, value: data})
	}
	sort.Slice(xs, func(i, j int) bool {
		a, b := xs[i], xs[j]
		if a.index != b.index {
			return a.index < b.index
		}
		if len(a.name) != len(b.name) {
			return len(a.name) < len(b.name)
		}
		return a.name < b.name
	})
	return xs, nil
}

// posixACLToDisk converts the ACL as exposed by getxattr (version 2, eight
// bytes per entry) to the on-disk format of ext4 (version 1, four bytes per
// entry unless the entry has an id).
func posixACLToDisk(in []byte) ([]byte, error) {
	const (
		userObj  = 0x01
		user     = 0x02
		groupObj = 0x04
		group    = 0x08
		mask     = 0x10
		other    = 0x20
	)
	if len(in) < 4 || binary.LittleEndian.Uint32(in) != 2 || (len(in)-4)%8 != 0 {
		return nil, fmt.Errorf("malformed posix ACL")
	}
	out := binary.LittleEndian.AppendUint32(nil, 1)
	for rest := in[4:]; len(rest) > 0; rest = rest[8:] {
		tag := binary.LittleEndian.Uint16(rest)
		perm := binary.LittleEndian.Uint16(rest[2:])
		id := binary.LittleEndian.Uint32(rest[4:])
		out = binary.LittleEndian.AppendUint16(out, tag)
		out = binary.LittleEndian.AppendUint16(out, perm)
		switch tag {
		case userObj, groupObj, mask, other:
		case user, group:
			out = binary.LittleEndian.AppendUint32(out, id)
		default:
			return nil, fmt.Errorf("posix ACL entry with unknown tag %#x", tag)
		}
	}
	return out, nil
}

func pad4(n int) int { return (n + 3) &^ 3 }

// xattrArea returns the bytes entries and values need in an attribute area,
// not counting the header or the 4 byte terminator.
func xattrSize(xs []xattr) int {
	size := 0
	for _, x := range xs {
		size += xattrEntryHeader + pad4(len(x.name)) + pad4(len(x.value))
	}
	return size
}

// xattrFitsInode reports whether the attributes fit the space after the
// extra inode fields.
func xattrFitsInode(xs []xattr) bool {
	return 4+xattrSize(xs)+4 <= ibodySize
}

// xattrFitsBlock reports whether the attributes fit a single block.
func xattrFitsBlock(xs []xattr, blockSize int) bool {
	return xattrBlockHeader+xattrSize(xs)+4 <= blockSize
}

// xattrEntryHash is ext4_xattr_hash_entry.
func xattrEntryHash(x xattr) uint32 {
	var h uint32
	for i := 0; i < len(x.name); i++ {
		h = h<<5 ^ h>>27 ^ uint32(x.name[i])
	}
	padded := make([]byte, pad4(len(x.value)))
	copy(padded, x.value)
	for i := 0; i < len(padded); i += 4 {
		h = h<<16 ^ h>>16 ^ binary.LittleEndian.Uint32(padded[i:])
	}
	return h
}

// encodeXattrArea fills area with the entries of xs and their values. The
// entries start at entriesAt; value offsets are relative to base.
func encodeXattrArea(area []byte, xs []xattr, entriesAt, base int) (entryHashes []uint32) {
	valueAt := len(area)
	pos := entriesAt
	for _, x := range xs {
		valueAt -= pad4(len(x.value))
		hash := xattrEntryHash(x)
		entryHashes = append(entryHashes, hash)

		area[pos] = byte(len(x.name))
		area[pos+1] = x.index
		if len(x.value) > 0 {
			binary.LittleEndian.PutUint16(area[pos+2:], uint16(valueAt-base))
		}
		binary.LittleEndian.PutUint32(area[pos+8:], uint32(len(x.value)))
		binary.LittleEndian.PutUint32(area[pos+12:], hash)
		copy(area[pos+xattrEntryHeader:], x.name)
		copy(area[valueAt:], x.value)
		pos += xattrEntryHeader + pad4(len(x.name))
	}
	return entryHashes
}

// encodeInodeXattrs returns the in-inode attribute area (ibodySize bytes).
func encodeInodeXattrs(xs []xattr) []byte {
	area := make([]byte, ibodySize)
	binary.LittleEndian.PutUint32(area, xattrMagic)
	encodeXattrArea(area, xs, 4, 4)
	return area
}

// encodeXattrBlock returns the attribute block stored at block number blk.
func encodeXattrBlock(xs []xattr, blockSize int, blk uint64, fsSeed uint32) []byte {
	block := make([]byte, blockSize)
	hashes := encodeXattrArea(block, xs, xattrBlockHeader, 0)

	var blockHash uint32
	for _, h := range hashes {
		if h == 0 {
			blockHash = 0
			break
		}
		blockHash = blockHash<<16 ^ blockHash>>16 ^ h
	}
	binary.LittleEndian.PutUint32(block[0:], xattrMagic)
	binary.LittleEndian.PutUint32(block[4:], 1) // reference count
	binary.LittleEndian.PutUint32(block[8:], 1) // blocks in use
	binary.LittleEndian.PutUint32(block[12:], blockHash)

	sum := crc32c(fsSeed, binary.LittleEndian.AppendUint64(nil, blk))
	binary.LittleEndian.PutUint32(block[16:], crc32c(sum, block))
	return block
}
