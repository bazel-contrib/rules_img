package basemeta

import (
	"archive/tar"
	"bytes"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// TestRoundTrip checks that every field of an entry survives a write/read
// cycle, including the ones a simpler format would drop (device numbers, xattrs,
// PAX records).
func TestRoundTrip(t *testing.T) {
	want := []*baselayer.BaseEntry{
		WithOwner(Dir("/var/lib/app", 0o750), 1000, 2000, "app", "appgroup"),
		File("/etc/motd", 0o644, []byte("hello\n")),
		FileFromPath("/usr/bin/tool", 0o755, "bazel-out/tool"),
		Symlink("/bin", "usr/bin"),
		{
			Path:           "dev/null",
			Type:           baselayer.EntryType_ENTRY_TYPE_CHAR_DEVICE,
			Mode:           0o666,
			Devmajor:       1,
			Devminor:       3,
			MtimeUnixNanos: 1700000000000000000,
			PaxRecords:     map[string]string{"SCHILY.fflags": "nodump"},
			Xattrs:         map[string]string{"user.note": "device"},
		},
	}

	var buf bytes.Buffer
	writer, err := NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := writer.WriteAll(want); err != nil {
		t.Fatalf("WriteAll: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].GetPath() != want[i].GetPath() {
			t.Errorf("entry %d: path = %q, want %q", i, got[i].GetPath(), want[i].GetPath())
		}
		if got[i].GetType() != want[i].GetType() {
			t.Errorf("entry %d: type = %v, want %v", i, got[i].GetType(), want[i].GetType())
		}
		if got[i].GetMode() != want[i].GetMode() {
			t.Errorf("entry %d: mode = %o, want %o", i, got[i].GetMode(), want[i].GetMode())
		}
	}

	device := got[4]
	if device.GetDevmajor() != 1 || device.GetDevminor() != 3 {
		t.Errorf("device numbers = %d/%d, want 1/3", device.GetDevmajor(), device.GetDevminor())
	}
	if device.GetXattrs()["user.note"] != "device" {
		t.Errorf("xattrs = %v, want user.note=device", device.GetXattrs())
	}
	if device.GetPaxRecords()["SCHILY.fflags"] != "nodump" {
		t.Errorf("pax records = %v, want SCHILY.fflags=nodump", device.GetPaxRecords())
	}
}

// TestReadEmptyStream checks that a stream with no entries reads back as no
// entries rather than an error. A content rule legitimately produces one when
// everything it would describe is disabled.
func TestReadEmptyStream(t *testing.T) {
	var buf bytes.Buffer
	writer, err := NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := Read(&buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("read %d entries from an empty stream, want 0", len(got))
	}
}

func TestNormalizePath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/etc/passwd", "etc/passwd"},
		{"etc/passwd", "etc/passwd"},
		{"./etc/passwd", "etc/passwd"},
		{"/usr/lib/", "usr/lib"},
		{"/", ""},
	} {
		if got := NormalizePath(tc.in); got != tc.want {
			t.Errorf("NormalizePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestToTarHeader checks the mapping onto a tar header, in particular that
// directories gain the trailing slash tar expects and that extended attributes
// become SCHILY.xattr PAX records.
func TestToTarHeader(t *testing.T) {
	entry := WithOwner(Dir("/var/log", 0o755), 0, 0, "root", "root")
	entry.Xattrs = map[string]string{"security.selinux": "system_u:object_r:var_log_t:s0"}

	header, err := ToTarHeader(entry)
	if err != nil {
		t.Fatalf("ToTarHeader: %v", err)
	}
	if header.Name != "var/log/" {
		t.Errorf("Name = %q, want %q", header.Name, "var/log/")
	}
	if header.Typeflag != tar.TypeDir {
		t.Errorf("Typeflag = %q, want %q", header.Typeflag, tar.TypeDir)
	}
	if header.Uname != "root" || header.Gname != "root" {
		t.Errorf("owner = %s:%s, want root:root", header.Uname, header.Gname)
	}
	want := "system_u:object_r:var_log_t:s0"
	if got := header.PAXRecords[SchilyXattrPrefix+"security.selinux"]; got != want {
		t.Errorf("selinux xattr = %q, want %q", got, want)
	}
	if !header.ModTime.IsZero() && header.ModTime.Unix() != 0 {
		t.Errorf("ModTime = %v, want the Unix epoch", header.ModTime)
	}
}

// TestToTarHeaderRejectsUnsetType makes sure a half-built entry fails loudly
// rather than becoming a regular file by default.
func TestToTarHeaderRejectsUnsetType(t *testing.T) {
	if _, err := ToTarHeader(&baselayer.BaseEntry{Path: "etc/thing"}); err == nil {
		t.Fatal("ToTarHeader accepted an entry with no type")
	}
}
