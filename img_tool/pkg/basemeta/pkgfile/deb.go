// Package pkgfile extracts files from Debian and RPM packages.
//
// This is deliberately not a package manager: it reads the payload archive and
// nothing else. No control metadata is parsed, no dependencies are resolved,
// no scripts are considered. The only use case is harvesting files that live at
// a well-known path inside a package, such as the CA certificates in
// ca-certificates.deb.
//
// Payloads compressed with xz are rejected rather than decompressed: a pure-Go
// xz decoder is not in the standard library, and the core img tool deliberately
// takes no dependency beyond go-containerregistry and the stdlib.
package pkgfile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Entry is one extracted regular file.
type Entry struct {
	// Path is the file's path inside the package, without a leading "/".
	Path string
	// Content is the file's bytes.
	Content []byte
}

// Matcher decides whether a path inside a package should be extracted.
type Matcher func(path string) bool

// PrefixMatcher matches any path under one of the given directory prefixes, or
// equal to one of them.
func PrefixMatcher(prefixes ...string) Matcher {
	cleaned := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		cleaned[i] = strings.TrimPrefix(prefix, "/")
	}
	return func(p string) bool {
		for _, prefix := range cleaned {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				return true
			}
		}
		return false
	}
}

// ErrUnsupportedCompression reports a payload this package cannot decompress.
type ErrUnsupportedCompression struct {
	// Package is the path of the package file.
	Package string
	// Compression names the format, e.g. "xz".
	Compression string
}

func (e *ErrUnsupportedCompression) Error() string {
	return fmt.Sprintf(
		"%s: payload is %s-compressed, which img cannot decompress; "+
			"recompress the package with gzip or zstd, or pass the certificates directly via the certs attribute",
		e.Package, e.Compression)
}

// ExtractDeb returns the files of a .deb package's data archive that match.
//
// A .deb is an ar archive holding, in order, debian-binary, control.tar* and
// data.tar*. Only the data member is read.
func ExtractDeb(debPath string, match Matcher) ([]Entry, error) {
	data, err := os.ReadFile(debPath)
	if err != nil {
		return nil, fmt.Errorf("reading deb: %w", err)
	}

	members, err := readAr(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", debPath, err)
	}

	for _, member := range members {
		if !strings.HasPrefix(member.name, "data.tar") {
			continue
		}
		if compression := unsupportedSuffix(member.name); compression != "" {
			return nil, &ErrUnsupportedCompression{Package: debPath, Compression: compression}
		}
		reader, err := decompress(bytes.NewReader(member.data), member.data)
		if err != nil {
			return nil, fmt.Errorf("%s: decompressing %s: %w", debPath, member.name, err)
		}
		entries, err := extractTar(reader, match)
		if err != nil {
			return nil, fmt.Errorf("%s: reading %s: %w", debPath, member.name, err)
		}
		return entries, nil
	}

	return nil, fmt.Errorf("%s: no data.tar member found (is this a Debian package?)", debPath)
}

// arMember is one member of an ar archive.
type arMember struct {
	name string
	data []byte
}

const arMagic = "!<arch>\n"

// readAr parses the common (BSD-flavoured) ar format that dpkg produces.
func readAr(data []byte) ([]arMember, error) {
	if len(data) < len(arMagic) || string(data[:len(arMagic)]) != arMagic {
		return nil, fmt.Errorf("not an ar archive (bad magic)")
	}
	offset := len(arMagic)

	var members []arMember
	for offset+60 <= len(data) {
		header := data[offset : offset+60]
		if string(header[58:60]) != "`\n" {
			return nil, fmt.Errorf("malformed ar header at offset %d", offset)
		}
		name := strings.TrimRight(strings.TrimSpace(string(header[0:16])), "/")
		size, err := parseDecimal(string(header[48:58]))
		if err != nil {
			return nil, fmt.Errorf("malformed ar member size at offset %d: %w", offset, err)
		}
		offset += 60
		if offset+int(size) > len(data) {
			return nil, fmt.Errorf("ar member %q claims %d bytes but the archive ends early", name, size)
		}
		members = append(members, arMember{name: name, data: data[offset : offset+int(size)]})
		offset += int(size)
		// Members are padded to an even offset.
		if size%2 == 1 {
			offset++
		}
	}
	return members, nil
}

func parseDecimal(field string) (int64, error) {
	field = strings.TrimSpace(field)
	var value int64
	if field == "" {
		return 0, fmt.Errorf("empty numeric field")
	}
	for _, c := range field {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%q is not a decimal number", field)
		}
		value = value*10 + int64(c-'0')
	}
	return value, nil
}

// unsupportedSuffix names the compression of a payload this package cannot
// read, or "" when the payload is supported.
func unsupportedSuffix(name string) string {
	switch path.Ext(name) {
	case ".xz":
		return "xz"
	case ".lzma":
		return "lzma"
	case ".bz2":
		return "bzip2"
	}
	return ""
}

// gzipMagic, zstdMagic and xzMagic identify a payload by its first bytes, for
// the formats (like RPM) that do not name their compression in a file name.
var (
	gzipMagic  = []byte{0x1f, 0x8b}
	zstdMagic  = []byte{0x28, 0xb5, 0x2f, 0xfd}
	xzMagic    = []byte{0xfd, '7', 'z', 'X', 'Z', 0x00}
	bzip2Magic = []byte{'B', 'Z', 'h'}
)

// decompress wraps r in the decompressor its magic bytes call for. peek must
// hold at least the first six bytes of the stream.
func decompress(r io.Reader, peek []byte) (io.Reader, error) {
	switch {
	case bytes.HasPrefix(peek, gzipMagic):
		return gzip.NewReader(r)
	case bytes.HasPrefix(peek, zstdMagic):
		decoder, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return decoder.IOReadCloser(), nil
	case bytes.HasPrefix(peek, xzMagic):
		return nil, fmt.Errorf("payload is xz-compressed")
	case bytes.HasPrefix(peek, bzip2Magic):
		return nil, fmt.Errorf("payload is bzip2-compressed")
	default:
		// Uncompressed payloads are legal in both formats.
		return r, nil
	}
}

// extractTar returns the regular files of a tar stream that match.
func extractTar(r io.Reader, match Matcher) ([]Entry, error) {
	var entries []Entry
	tr := tar.NewReader(r)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		name := normalizeMemberPath(header.Name)
		if !match(name) {
			continue
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", name, err)
		}
		entries = append(entries, Entry{Path: name, Content: content})
	}
	return entries, nil
}

// normalizeMemberPath strips the "./" prefix and leading slash that package
// payloads use, so callers can match on plain paths.
func normalizeMemberPath(name string) string {
	name = strings.TrimPrefix(name, "./")
	return strings.TrimPrefix(name, "/")
}
