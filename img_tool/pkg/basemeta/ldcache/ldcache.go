// Package ldcache writes the glibc dynamic loader cache (/etc/ld.so.cache).
//
// Only the "new" format is written: glibc has emitted it since 2.2, and a cache
// with no old-format section is what `ldconfig -X` produces on any current
// distribution. musl ignores the file entirely and resolves libraries from
// /etc/ld-musl-*.path instead, so a musl image simply does not need one.
package ldcache

import (
	"bytes"
	"encoding/binary"
	"sort"
	"strings"
)

// Header constants from glibc's sysdeps/generic/dl-cache.h. The magic and
// version are adjacent fixed-size fields with no NUL terminator between them,
// so together they occupy exactly 20 bytes.
const (
	cacheMagicNew = "glibc-ld.so.cache"
	cacheVersion  = "1.1"

	// headerSize is magic[17] + version[3] + nlibs + len_strings + flags and
	// its padding + extension_offset + unused[3].
	headerSize = 48
	// recordSize is sizeof(struct file_entry_new).
	recordSize = 24
)

// Entry flags and fields from dl-cache.h.
const (
	// flagElfLibc6 marks an entry as an ELF object for glibc 2.x, which is
	// everything a container image holds.
	flagElfLibc6 = 0x03
	// hwcapUnset leaves the hwcap field empty: entries are not tied to a
	// specific CPU feature set.
	hwcapUnset = 0

	// flagsEndianLittle and flagsEndianBig are the endianness bits of the
	// header's flags byte. glibc also accepts a zero flags byte as "endianness
	// unknown", but stating it lets a mismatched cache be diagnosed instead of
	// silently misread.
	flagsEndianLittle = 2
	flagsEndianBig    = 3
)

// Entry is one library the cache should resolve.
type Entry struct {
	// SONAME is the name a binary asks for, e.g. "libssl.so.3".
	SONAME string
	// Path is the absolute path of the library inside the image.
	Path string
	// OSVersion is the minimum OS version required, almost always 0.
	OSVersion uint32
}

// Write renders the entries as an ld.so.cache file for a target of the given
// byte order. The cache is read with the target's native integer layout, so
// building an image for a big-endian target (s390x) requires the matching
// order here.
//
// glibc looks entries up with a binary search over the record array, so the
// records are sorted by SONAME. The string table is deduplicated: the same path
// is usually referenced by both a versioned SONAME and a development symlink.
func Write(entries []Entry, order binary.ByteOrder) []byte {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].SONAME != sorted[j].SONAME {
			return sorted[i].SONAME < sorted[j].SONAME
		}
		return sorted[i].Path < sorted[j].Path
	})

	var strtab bytes.Buffer
	offsets := make(map[string]uint32)
	// String offsets are relative to the start of the file, not to the table,
	// so the table's base must be known before interning anything.
	stringTableBase := uint32(headerSize + recordSize*len(sorted))
	intern := func(s string) uint32 {
		if offset, seen := offsets[s]; seen {
			return offset
		}
		offset := stringTableBase + uint32(strtab.Len())
		offsets[s] = offset
		strtab.WriteString(s)
		strtab.WriteByte(0)
		return offset
	}

	type record struct{ key, value, osVersion uint32 }
	records := make([]record, 0, len(sorted))
	for _, entry := range sorted {
		records = append(records, record{
			key:       intern(entry.SONAME),
			value:     intern(entry.Path),
			osVersion: entry.OSVersion,
		})
	}

	endianFlag := byte(flagsEndianLittle)
	if order == binary.BigEndian {
		endianFlag = flagsEndianBig
	}

	var out bytes.Buffer
	out.Grow(int(stringTableBase) + strtab.Len())

	out.WriteString(cacheMagicNew)                  // magic[17], not NUL-terminated
	out.WriteString(cacheVersion)                   // version[3], not NUL-terminated
	binary.Write(&out, order, uint32(len(sorted)))  // nlibs
	binary.Write(&out, order, uint32(strtab.Len())) // len_strings
	out.WriteByte(endianFlag)                       // flags
	out.Write(make([]byte, 3))                      // padding_unused[3]
	binary.Write(&out, order, uint32(0))            // extension_offset: none
	out.Write(make([]byte, 12))                     // unused[3]

	for _, r := range records {
		binary.Write(&out, order, int32(flagElfLibc6))
		binary.Write(&out, order, r.key)
		binary.Write(&out, order, r.value)
		binary.Write(&out, order, r.osVersion)
		binary.Write(&out, order, uint64(hwcapUnset))
	}

	out.Write(strtab.Bytes())
	return out.Bytes()
}

// ConfContent renders an /etc/ld.so.conf.d fragment listing directories, one
// per line.
func ConfContent(directories []string) []byte {
	sorted := make([]string, len(directories))
	copy(sorted, directories)
	sort.Strings(sorted)
	return []byte(strings.Join(dedupeSorted(sorted), "\n") + "\n")
}

func dedupeSorted(values []string) []string {
	out := values[:0]
	var previous string
	for i, value := range values {
		if i > 0 && value == previous {
			continue
		}
		previous = value
		out = append(out, value)
	}
	return out
}
