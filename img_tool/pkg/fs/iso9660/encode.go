package iso9660

import (
	"fmt"
	"time"
)

// The format stores most numbers twice, once little endian and once big
// endian, so that a reader of either byte order can use the image without
// swapping. ECMA-119 calls these the "both byte orders" fields.

func putU16LE(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }
func putU16BE(b []byte, v uint16) { b[0], b[1] = byte(v>>8), byte(v) }

func putU32LE(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

func putU32BE(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

func putU64LE(b []byte, v uint64) {
	putU32LE(b[0:], uint32(v))
	putU32LE(b[4:], uint32(v>>32))
}

// putBoth16 and putBoth32 write a number in both byte orders.
func putBoth16(b []byte, v uint16) {
	putU16LE(b[0:], v)
	putU16BE(b[2:], v)
}

func putBoth32(b []byte, v uint32) {
	putU32LE(b[0:], v)
	putU32BE(b[4:], v)
}

func appendBoth32(b []byte, v uint32) []byte {
	var buf [8]byte
	putBoth32(buf[:], v)
	return append(b, buf[:]...)
}

// appendShortTime appends the seven byte recording time of a directory record
// or a Rock Ridge timestamp. The year is stored as an offset from 1900, so
// times outside 1900 to 2155 are clamped.
func appendShortTime(b []byte, t time.Time) []byte {
	t = t.UTC()
	if t.Year() < 1900 {
		t = time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	if t.Year() > 2155 {
		t = time.Date(2155, 12, 31, 23, 59, 59, 0, time.UTC)
	}
	return append(b,
		byte(t.Year()-1900),
		byte(t.Month()),
		byte(t.Day()),
		byte(t.Hour()),
		byte(t.Minute()),
		byte(t.Second()),
		0, // offset from UTC in 15 minute intervals
	)
}

// putLongTime writes the seventeen character date and time of a volume
// descriptor.
func putLongTime(b []byte, t time.Time) {
	t = t.UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		t = time.Unix(0, 0).UTC()
	}
	copy(b, fmt.Sprintf("%04d%02d%02d%02d%02d%02d%02d",
		t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/10_000_000))
	b[16] = 0 // offset from UTC in 15 minute intervals
}

// putPadded writes s into a fixed width field, padded with pad and truncated
// if it is too long.
func putPadded(b []byte, s string, pad byte) {
	n := copy(b, s)
	for i := n; i < len(b); i++ {
		b[i] = pad
	}
}

// putPaddedUCS2 writes s into a fixed width field as big-endian UCS-2, padded
// with UCS-2 spaces. A field that cannot hold the whole string is truncated on
// a character boundary.
func putPaddedUCS2(b []byte, s string) {
	encoded := ucs2(truncateUTF16(s, len(b)/2))
	n := copy(b, encoded)
	for i := n; i+1 < len(b); i += 2 {
		b[i], b[i+1] = 0x00, 0x20
	}
}

// blocksFor is the number of logical blocks that hold n bytes.
func blocksFor(n int64) uint32 {
	return uint32((n + BlockSize - 1) / BlockSize)
}
