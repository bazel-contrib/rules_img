package fat32

import (
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// invalidNameChars are the characters that may not occur in a long file name.
const invalidNameChars = `"*/:<>?\|`

// validateName checks the name of a file or directory.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("empty name")
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("name %q is not valid UTF-8", name)
	}
	for _, r := range name {
		if r < 0x20 || strings.ContainsRune(invalidNameChars, r) {
			return fmt.Errorf("name %q contains the character %q, which FAT cannot store", name, r)
		}
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return fmt.Errorf("name %q ends with a dot or a space, which FAT does not preserve", name)
	}
	if n := len(utf16.Encode([]rune(name))); n > maxNameLength {
		return fmt.Errorf("name %q is %d UTF-16 characters long, longer than %d", name, n, maxNameLength)
	}
	return nil
}

func validateLabel(label string) error {
	if len(label) > 11 {
		return fmt.Errorf("fat32: label %q is longer than 11 characters", label)
	}
	for i := 0; i < len(label); i++ {
		if c := label[i]; c != ' ' && !isShortChar(c) {
			return fmt.Errorf("fat32: label %q contains %q, which is not allowed in a label", label, c)
		}
	}
	return nil
}

// foldName is how names are compared: FAT is case insensitive.
func foldName(name string) string { return strings.ToUpper(name) }

// isShortChar reports whether c may occur in a short name (upper case).
func isShortChar(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("$%'-_@~`!(){}^#&", c) >= 0
}

// shortNameOf returns the 11 bytes of a short directory entry name, with the
// base padded to 8 and the extension to 3 characters.
func shortNameOf(base, ext string) [11]byte {
	var n [11]byte
	for i := range n {
		n[i] = ' '
	}
	copy(n[:8], base)
	copy(n[8:], ext)
	return n
}

// displayShort is the name of a short entry as it is looked up: "BASE.EXT".
func displayShort(n [11]byte) string {
	base := strings.TrimRight(string(n[:8]), " ")
	ext := strings.TrimRight(string(n[8:]), " ")
	if ext == "" {
		return base
	}
	return base + "." + ext
}

// exactShort returns the short name of name if name is, apart from its case,
// already a valid short name.
func exactShort(name string) (short [11]byte, ok bool) {
	base, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		base, ext = name[:i], name[i+1:]
	}
	if base == "" || len(base) > 8 || len(ext) > 3 || strings.ContainsRune(base, '.') || strings.ContainsRune(ext, '.') {
		return short, false
	}
	upper := strings.ToUpper(name)
	for i := 0; i < len(upper); i++ {
		if c := upper[i]; c != '.' && !isShortChar(c) {
			return short, false
		}
	}
	u := strings.ToUpper(base)
	return shortNameOf(u, strings.ToUpper(ext)), true
}

// generateShort derives a short name for a name that is not a valid one, with
// a numeric tail that makes it unique: the base is cut to 6 characters (fewer
// with a longer tail) followed by "~N".
func generateShort(name string, taken func(string) bool) ([11]byte, error) {
	name = strings.ToUpper(strings.ReplaceAll(name, " ", ""))
	name = strings.TrimLeft(name, ".")
	base, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		base, ext = name[:i], name[i+1:]
	}
	base = strings.ReplaceAll(base, ".", "")
	clean := func(s string, limit int) string {
		var b []byte
		for _, r := range s {
			if len(b) == limit {
				break
			}
			if r < 0x80 && isShortChar(byte(r)) {
				b = append(b, byte(r))
			} else {
				b = append(b, '_') // not representable in a short name
			}
		}
		return string(b)
	}
	ext = clean(ext, 3)
	if base == "" {
		base, ext = ext, "" // only an extension, such as ".gitignore"
	}
	for n := 1; n < 1_000_000; n++ {
		tail := fmt.Sprintf("~%d", n)
		short := shortNameOf(clean(base, 8-len(tail))+tail, ext)
		if !taken(displayShort(short)) {
			return short, nil
		}
	}
	return [11]byte{}, fmt.Errorf("no short name left for %q", name)
}

// shortChecksum is the checksum of a short name that long name entries carry.
func shortChecksum(name [11]byte) byte {
	var sum byte
	for _, c := range name {
		sum = ((sum&1)<<7 | sum>>1) + c
	}
	return sum
}

// longEntries encodes name as the sequence of long name directory entries
// that precede its short entry, in the order they are stored.
func longEntries(name string, checksum byte) [][32]byte {
	units := utf16.Encode([]rune(name))
	count := (len(units) + 12) / 13
	padded := make([]uint16, count*13)
	for i := range padded {
		switch {
		case i < len(units):
			padded[i] = units[i]
		case i == len(units):
			padded[i] = 0 // terminator
		default:
			padded[i] = 0xFFFF
		}
	}
	entries := make([][32]byte, count)
	for e := 0; e < count; e++ {
		var d [32]byte
		ord := byte(e + 1)
		if e == count-1 {
			ord |= 0x40
		}
		d[0] = ord
		d[11] = 0x0F
		d[13] = checksum
		chars := padded[e*13 : e*13+13]
		put := func(off int, vals []uint16) {
			for i, v := range vals {
				d[off+2*i] = byte(v)
				d[off+2*i+1] = byte(v >> 8)
			}
		}
		put(1, chars[0:5])
		put(14, chars[5:11])
		put(28, chars[11:13])
		entries[count-1-e] = d // the last part is stored first
	}
	return entries
}
