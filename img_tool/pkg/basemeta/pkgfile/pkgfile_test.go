package pkgfile

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// tarPayload builds a tar archive holding the given path/content pairs.
func tarPayload(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// A real payload leads with "./" paths and directory entries; include both
	// so the normalization is exercised.
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "./usr/", Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for path, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     "./" + path,
			Mode:     0o644,
			Size:     int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zstdBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeDeb assembles a .deb: an ar archive of debian-binary, control and data.
func writeDeb(t *testing.T, dataName string, data []byte) string {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString(arMagic)

	member := func(name string, content []byte) {
		// name[16] mtime[12] uid[6] gid[6] mode[8] size[10] magic[2]
		fmt.Fprintf(&buf, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", name, 0, 0, 0, "100644", len(content))
		buf.Write(content)
		if len(content)%2 == 1 {
			buf.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.gz", gzipBytes(t, tarPayload(t, map[string]string{"control": "Package: test\n"})))
	member(dataName, data)

	path := filepath.Join(t.TempDir(), "test.deb")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestExtractDeb checks that the data member is found and that only matching
// paths come back, regardless of payload compression.
func TestExtractDeb(t *testing.T) {
	payload := tarPayload(t, map[string]string{
		"usr/share/ca-certificates/acme/root.crt": "ROOT",
		"usr/share/doc/ca-certificates/README":    "not a certificate",
	})

	for _, tc := range []struct {
		name     string
		dataName string
		data     []byte
	}{
		{"gzip", "data.tar.gz", gzipBytes(t, payload)},
		{"zstd", "data.tar.zst", zstdBytes(t, payload)},
		{"uncompressed", "data.tar", payload},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := ExtractDeb(writeDeb(t, tc.dataName, tc.data), PrefixMatcher("usr/share/ca-certificates"))
			if err != nil {
				t.Fatalf("ExtractDeb: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("extracted %d entries, want 1", len(entries))
			}
			if entries[0].Path != "usr/share/ca-certificates/acme/root.crt" {
				t.Errorf("path = %q", entries[0].Path)
			}
			if string(entries[0].Content) != "ROOT" {
				t.Errorf("content = %q, want ROOT", entries[0].Content)
			}
		})
	}
}

// TestExtractDebRejectsXZ checks that an xz payload produces an actionable
// error rather than a silent miss. Supporting xz would mean taking on a
// dependency the core tool deliberately does without.
func TestExtractDebRejectsXZ(t *testing.T) {
	path := writeDeb(t, "data.tar.xz", []byte{0xfd, '7', 'z', 'X', 'Z', 0x00})
	_, err := ExtractDeb(path, PrefixMatcher("usr/share/ca-certificates"))
	if err == nil {
		t.Fatal("ExtractDeb accepted an xz payload")
	}
	var unsupported *ErrUnsupportedCompression
	if !errorsAs(err, &unsupported) {
		t.Fatalf("error %v is not an ErrUnsupportedCompression", err)
	}
	if !strings.Contains(err.Error(), "recompress") {
		t.Errorf("error %q does not tell the user what to do about it", err)
	}
}

// TestExtractDebRejectsNonAr checks the magic-byte guard.
func TestExtractDebRejectsNonAr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fake.deb")
	if err := os.WriteFile(path, []byte("not an ar archive at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ExtractDeb(path, PrefixMatcher("usr")); err == nil {
		t.Fatal("ExtractDeb accepted a file that is not an ar archive")
	}
}

// cpioEntry appends one newc record.
func cpioEntry(buf *bytes.Buffer, name string, mode uint64, content []byte) {
	name += "\x00"
	fmt.Fprintf(buf, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		1 /*ino*/, mode, 0 /*uid*/, 0 /*gid*/, 1 /*nlink*/, 0 /*mtime*/, len(content),
		0, 0, 0, 0, len(name), 0 /*check*/)
	buf.WriteString(name)
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
	buf.Write(content)
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

// writeRPM assembles a minimal .rpm: a lead, two headers and a cpio payload.
func writeRPM(t *testing.T, compressor string, payload []byte) string {
	t.Helper()
	var buf bytes.Buffer

	// Lead: magic plus 92 bytes the reader skips over.
	buf.Write(rpmLeadMagic)
	buf.Write(make([]byte, rpmLeadSize-len(rpmLeadMagic)))

	writeHeader := func(tags map[int32]string) {
		var store bytes.Buffer
		type index struct {
			tag    int32
			offset uint32
		}
		var entries []index
		for tag, value := range tags {
			entries = append(entries, index{tag: tag, offset: uint32(store.Len())})
			store.WriteString(value)
			store.WriteByte(0)
		}

		buf.Write(rpmHeaderMagic)
		buf.WriteByte(1)           // version
		buf.Write(make([]byte, 4)) // reserved
		binary.Write(&buf, binary.BigEndian, uint32(len(entries)))
		binary.Write(&buf, binary.BigEndian, uint32(store.Len()))
		for _, entry := range entries {
			binary.Write(&buf, binary.BigEndian, uint32(entry.tag))
			binary.Write(&buf, binary.BigEndian, uint32(rpmTypeString))
			binary.Write(&buf, binary.BigEndian, entry.offset)
			binary.Write(&buf, binary.BigEndian, uint32(1)) // count
		}
		buf.Write(store.Bytes())
	}

	writeHeader(nil) // signature header
	for buf.Len()%8 != 0 {
		buf.WriteByte(0)
	}
	writeHeader(map[int32]string{
		rpmTagPayloadCompressor: compressor,
		rpmTagPayloadFormat:     "cpio",
	})
	buf.Write(payload)

	path := filepath.Join(t.TempDir(), "test.rpm")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestExtractRPM checks header parsing, cpio walking and path matching.
func TestExtractRPM(t *testing.T) {
	var cpio bytes.Buffer
	cpioEntry(&cpio, "./etc/pki/ca-trust/source/anchors/root.pem", cpioModeRegular|0o644, []byte("ROOT"))
	cpioEntry(&cpio, "./etc/pki/ca-trust/source", 0o040000|0o755, nil) // a directory, skipped
	cpioEntry(&cpio, "./usr/share/doc/README", cpioModeRegular|0o644, []byte("prose"))
	cpioEntry(&cpio, cpioTrailer, 0, nil)

	entries, err := ExtractRPM(
		writeRPM(t, "gzip", gzipBytes(t, cpio.Bytes())),
		PrefixMatcher("etc/pki/ca-trust/source"),
	)
	if err != nil {
		t.Fatalf("ExtractRPM: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("extracted %d entries, want 1 (directories and unmatched paths should be skipped)", len(entries))
	}
	if entries[0].Path != "etc/pki/ca-trust/source/anchors/root.pem" {
		t.Errorf("path = %q", entries[0].Path)
	}
	if string(entries[0].Content) != "ROOT" {
		t.Errorf("content = %q, want ROOT", entries[0].Content)
	}
}

// TestExtractRPMRejectsXZ checks that the compressor tag is honoured before any
// attempt to read the payload.
func TestExtractRPMRejectsXZ(t *testing.T) {
	_, err := ExtractRPM(writeRPM(t, "xz", []byte("whatever")), PrefixMatcher("etc"))
	if err == nil {
		t.Fatal("ExtractRPM accepted an xz payload")
	}
	var unsupported *ErrUnsupportedCompression
	if !errorsAs(err, &unsupported) {
		t.Fatalf("error %v is not an ErrUnsupportedCompression", err)
	}
}

// TestPrefixMatcher checks that a prefix matches a directory and its contents
// but not a sibling whose name merely starts the same way.
func TestPrefixMatcher(t *testing.T) {
	match := PrefixMatcher("/etc/ssl/certs")
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"etc/ssl/certs", true},
		{"etc/ssl/certs/ca.pem", true},
		{"etc/ssl/certs-backup/ca.pem", false},
		{"etc/ssl", false},
	} {
		if got := match(tc.path); got != tc.want {
			t.Errorf("match(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// errorsAs is a local errors.As to keep the import list of this test small.
func errorsAs(err error, target **ErrUnsupportedCompression) bool {
	for err != nil {
		if typed, ok := err.(*ErrUnsupportedCompression); ok {
			*target = typed
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
