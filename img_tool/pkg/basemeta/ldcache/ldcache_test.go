package ldcache

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestWriteHeader pins the header layout glibc's dl-cache.c reads.
//
// The magic and version are adjacent fixed-size fields with no separator, and
// every offset in the record array is relative to the start of the file. Both
// are easy to get subtly wrong, and the result is a cache the loader silently
// ignores (or, worse, misreads).
func TestWriteHeader(t *testing.T) {
	out := Write([]Entry{
		{SONAME: "libssl.so.3", Path: "/usr/lib/libssl.so.3"},
	}, binary.LittleEndian)

	if got := string(out[:len(cacheMagicNew)]); got != cacheMagicNew {
		t.Errorf("magic = %q, want %q", got, cacheMagicNew)
	}
	versionStart := len(cacheMagicNew)
	if got := string(out[versionStart : versionStart+len(cacheVersion)]); got != cacheVersion {
		t.Errorf("version = %q, want %q", got, cacheVersion)
	}

	nlibs := binary.LittleEndian.Uint32(out[20:24])
	if nlibs != 1 {
		t.Errorf("nlibs = %d, want 1", nlibs)
	}
	lenStrings := binary.LittleEndian.Uint32(out[24:28])
	if int(lenStrings) != len("libssl.so.3\x00/usr/lib/libssl.so.3\x00") {
		t.Errorf("len_strings = %d, want the size of the string table", lenStrings)
	}
	if out[28] != flagsEndianLittle {
		t.Errorf("flags = %d, want %d for a little-endian target", out[28], flagsEndianLittle)
	}

	// The record follows the 48-byte header; its key and value are file offsets.
	record := out[headerSize : headerSize+recordSize]
	if flags := int32(binary.LittleEndian.Uint32(record[0:4])); flags != flagElfLibc6 {
		t.Errorf("entry flags = %d, want %d", flags, flagElfLibc6)
	}
	key := binary.LittleEndian.Uint32(record[4:8])
	value := binary.LittleEndian.Uint32(record[8:12])
	if got := readCString(out, key); got != "libssl.so.3" {
		t.Errorf("key resolves to %q, want the SONAME", got)
	}
	if got := readCString(out, value); got != "/usr/lib/libssl.so.3" {
		t.Errorf("value resolves to %q, want the path", got)
	}
}

// TestWriteSortsBySONAME checks the invariant glibc's binary search depends on.
func TestWriteSortsBySONAME(t *testing.T) {
	out := Write([]Entry{
		{SONAME: "libz.so.1", Path: "/usr/lib/libz.so.1"},
		{SONAME: "libc.so.6", Path: "/usr/lib/libc.so.6"},
		{SONAME: "libm.so.6", Path: "/usr/lib/libm.so.6"},
	}, binary.LittleEndian)

	var previous string
	for i := 0; i < 3; i++ {
		record := out[headerSize+i*recordSize:]
		soname := readCString(out, binary.LittleEndian.Uint32(record[4:8]))
		if i > 0 && soname <= previous {
			t.Errorf("entry %d (%q) is not sorted after %q", i, soname, previous)
		}
		previous = soname
	}
}

// TestWriteDeduplicatesStrings checks that a string referenced twice is stored
// once, which is the common case when a SONAME equals its file name.
func TestWriteDeduplicatesStrings(t *testing.T) {
	out := Write([]Entry{
		{SONAME: "libc.so.6", Path: "libc.so.6"},
	}, binary.LittleEndian)

	record := out[headerSize:]
	key := binary.LittleEndian.Uint32(record[4:8])
	value := binary.LittleEndian.Uint32(record[8:12])
	if key != value {
		t.Errorf("identical strings got distinct offsets %d and %d", key, value)
	}
}

// TestWriteBigEndian checks that a big-endian target gets a big-endian cache;
// the loader reads it with native integers, so the wrong order is unreadable.
func TestWriteBigEndian(t *testing.T) {
	out := Write([]Entry{{SONAME: "libc.so.6", Path: "/lib/libc.so.6"}}, binary.BigEndian)

	if out[28] != flagsEndianBig {
		t.Errorf("flags = %d, want %d for a big-endian target", out[28], flagsEndianBig)
	}
	if nlibs := binary.BigEndian.Uint32(out[20:24]); nlibs != 1 {
		t.Errorf("nlibs read big-endian = %d, want 1", nlibs)
	}
}

func TestConfContent(t *testing.T) {
	got := string(ConfContent([]string{"/usr/lib/x86_64-linux-gnu", "/usr/lib", "/usr/lib"}))
	want := "/usr/lib\n/usr/lib/x86_64-linux-gnu\n"
	if got != want {
		t.Errorf("ConfContent = %q, want %q (sorted and deduplicated)", got, want)
	}
}

// readCString reads the NUL-terminated string at a file offset.
func readCString(data []byte, offset uint32) string {
	if int(offset) >= len(data) {
		return ""
	}
	rest := data[offset:]
	if end := bytes.IndexByte(rest, 0); end >= 0 {
		return string(rest[:end])
	}
	return strings.TrimRight(string(rest), "\x00")
}
