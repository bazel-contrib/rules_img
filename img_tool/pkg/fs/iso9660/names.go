package iso9660

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxComponentLength is the longest path component the writer accepts. Rock
// Ridge and UDF can describe longer names, but nothing produces them and the
// limit keeps a single entry's metadata inside one logical block.
const maxComponentLength = 255

// validateName checks one path component.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("empty name")
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("name %q is not valid UTF-8", name)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("name %q contains a NUL byte", name)
	}
	if len(name) > maxComponentLength {
		return fmt.Errorf("name %q is %d bytes long, longer than %d", name, len(name), maxComponentLength)
	}
	return nil
}

// isDChar reports whether c is one of the d-characters ISO 9660 allows in an
// identifier: upper case letters, digits and the underscore.
func isDChar(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		return true
	}
	return false
}

// mangleDChars maps a name to the d-characters ISO 9660 allows, cutting it to
// limit characters. Everything that is not a d-character becomes an
// underscore, which is what every other writer does.
func mangleDChars(name string, limit int) string {
	var b strings.Builder
	for i := 0; i < len(name) && b.Len() < limit; i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if !isDChar(c) {
			c = '_'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// isoIdentifier derives the identifier of an entry in the primary hierarchy,
// which is what a reader that uses neither Rock Ridge nor Joliet nor UDF sees.
// Names are upper case d-characters; a file keeps at most one dot and carries
// the ";1" version every file identifier needs.
//
// taken reports whether an identifier is already used in the same directory. A
// clash is resolved by a decimal counter in the base, so the result is unique
// and depends only on the names of the directory and their order.
func isoIdentifier(name string, isDir bool, taken func(string) bool) (string, error) {
	if isDir {
		base := mangleDChars(strings.ReplaceAll(name, ".", "_"), maxDirIdentifier)
		if base == "" {
			base = "_"
		}
		return unique(base, "", maxDirIdentifier, byteMetric, taken)
	}

	base, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 && i < len(name)-1 {
		base, ext = name[:i], name[i+1:]
	}
	// The version suffix is not part of the 30 characters of the identifier.
	ext = mangleDChars(strings.ReplaceAll(ext, ".", "_"), 3)
	room := maxISOIdentifier
	if ext != "" {
		room -= len(ext) + 1
	}
	if room < 1 {
		return "", fmt.Errorf("name %q has no room left for a file identifier", name)
	}
	mangled := mangleDChars(strings.ReplaceAll(base, ".", "_"), room)
	if mangled == "" {
		mangled = "_"
	}
	if ext != "" {
		ext = "." + ext
	}
	return unique(mangled, ext+";1", room, byteMetric, taken)
}

// nameMetric is how the length of an identifier is counted, and how one is
// shortened to a length: the limits of the primary hierarchy are in bytes of
// d-characters, those of Joliet in UTF-16 code units.
type nameMetric struct {
	length func(string) int
	cut    func(string, int) string
}

var (
	byteMetric = nameMetric{
		length: func(s string) int { return len(s) },
		cut:    func(s string, n int) string { return s[:n] },
	}
	utf16Metric = nameMetric{length: utf16Len, cut: truncateUTF16}
)

// unique returns base+suffix, or, if that is taken, a base with a decimal
// counter appended (replacing its tail if the base is already as long as room
// allows). The counter is ASCII, so it is one unit per digit in either metric.
func unique(base, suffix string, room int, metric nameMetric, taken func(string) bool) (string, error) {
	if !taken(base + suffix) {
		return base + suffix, nil
	}
	for n := 1; n < 1_000_000; n++ {
		tail := strconv.Itoa(n)
		head := base
		if metric.length(head)+len(tail) > room {
			head = metric.cut(head, max(room-len(tail), 0))
		}
		if candidate := head + tail + suffix; !taken(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no identifier left for %q", base+suffix)
}

// jolietForbidden are the characters a Joliet name may not contain.
const jolietForbidden = `*/:;?\`

// jolietIdentifier derives the UCS-2 name of an entry in the Joliet
// hierarchy. Unlike the primary hierarchy, Joliet keeps the real name, with
// only the handful of characters the specification forbids replaced and the
// length capped.
func jolietIdentifier(name string, limit int, taken func(string) bool) (string, error) {
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(jolietForbidden, r) {
			r = '_'
		}
		b.WriteRune(r)
	}
	cut := truncateUTF16(b.String(), limit)
	if cut == "" {
		cut = "_"
	}
	return unique(cut, "", limit, utf16Metric, taken)
}

// utf16Len is how many UTF-16 code units a string needs, which is the unit the
// Joliet name limit is counted in.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// truncateUTF16 cuts s to at most limit UTF-16 code units, never splitting a
// surrogate pair.
func truncateUTF16(s string, limit int) string {
	units := 0
	for i, r := range s {
		n := 1
		if r > 0xFFFF {
			n = 2
		}
		if units+n > limit {
			return s[:i]
		}
		units += n
	}
	return s
}

// ucs2 encodes a string as the big-endian UCS-2 Joliet stores. Code points
// outside the basic multilingual plane become surrogate pairs, which is UTF-16
// rather than UCS-2, and is what every implementation does.
func ucs2(s string) []byte {
	units := utf16.Encode([]rune(s))
	out := make([]byte, 2*len(units))
	for i, u := range units {
		out[2*i] = byte(u >> 8)
		out[2*i+1] = byte(u)
	}
	return out
}

// identifierUnit is the width of one character of a hierarchy's identifiers:
// a byte for the d-characters of the primary hierarchy, two bytes for the
// big-endian UCS-2 of Joliet.
func identifierUnit(joliet bool) int {
	if joliet {
		return 2
	}
	return 1
}

// compareIdentifiers orders two directory record identifiers the way ISO 9660
// requires: character by character, with the shorter one padded with spaces.
//
// unit is the width of a character, which decides what a space is: padding a
// Joliet identifier with single 0x20 bytes instead of the UCS-2 space 0x0020
// would sort a name after every name it is a prefix of.
func compareIdentifiers(a, b []byte, unit int) int {
	space := []byte{0x20}
	if unit == 2 {
		space = []byte{0x00, 0x20}
	}
	at := func(p []byte, i int) []byte {
		if (i+1)*unit <= len(p) {
			return p[i*unit : (i+1)*unit]
		}
		return space
	}
	for i := range (max(len(a), len(b)) + unit - 1) / unit {
		if c := bytes.Compare(at(a, i), at(b, i)); c != 0 {
			return c
		}
	}
	return 0
}
