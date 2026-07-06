package cst

import (
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/mtree"
)

func ptrStrings(s []string) *[]string { return &s }
func ptrInt64(v int64) *int64         { return &v }

func configFile() *v1.ConfigFile {
	return &v1.ConfigFile{
		Config: v1.Config{
			Env:          []string{"PATH=/usr/bin", "FOO=bar"},
			Cmd:          []string{"/bin/sh", "-c"},
			Entrypoint:   []string{"/entry"},
			Labels:       map[string]string{"maintainer": "team@example.com"},
			ExposedPorts: map[string]struct{}{"8080/tcp": {}},
			Volumes:      map[string]struct{}{"/data": {}},
			WorkingDir:   "/app",
			User:         "nobody",
		},
	}
}

func failures(results []result) []result {
	var out []result
	for _, r := range results {
		if !r.pass {
			out = append(out, r)
		}
	}
	return out
}

func TestCheckMetadataPass(t *testing.T) {
	mt := MetadataTest{
		EnvVars:      []EnvVar{{Key: "PATH", Value: "/usr/bin"}, {Key: "FOO", Value: "^b.r$", IsRegex: true}},
		Labels:       []Label{{Key: "maintainer", Value: ".*@example.com", IsRegex: true}},
		Cmd:          ptrStrings([]string{"/bin/sh", "-c"}),
		Entrypoint:   ptrStrings([]string{"/entry"}),
		ExposedPorts: []string{"8080/tcp"},
		Volumes:      []string{"/data"},
		Workdir:      "/app",
		User:         "nobody",
	}
	if f := failures(checkMetadata(configFile(), mt)); len(f) != 0 {
		t.Errorf("unexpected failures: %+v", f)
	}
}

func TestCheckMetadataFailures(t *testing.T) {
	mt := MetadataTest{
		EnvVars:      []EnvVar{{Key: "MISSING", Value: "x"}, {Key: "PATH", Value: "/wrong"}},
		Cmd:          ptrStrings([]string{"/other"}),
		ExposedPorts: []string{"9090/tcp"},
		Workdir:      "/wrong",
	}
	f := failures(checkMetadata(configFile(), mt))
	if len(f) != 5 {
		t.Errorf("expected 5 failures, got %d: %+v", len(f), f)
	}
}

func TestExposedPortBareNumberAlias(t *testing.T) {
	mt := MetadataTest{ExposedPorts: []string{"8080"}}
	if f := failures(checkMetadata(configFile(), mt)); len(f) != 0 {
		t.Errorf("bare port 8080 should match 8080/tcp: %+v", f)
	}
}

func mtreeEntries() map[string]mtree.ParsedEntry {
	return map[string]mtree.ParsedEntry{
		"hello.txt": {Path: "hello.txt", Keywords: map[string]string{"type": "file", "mode": "0644", "uid": "0", "gid": "0"}},
		"bin/app":   {Path: "bin/app", Keywords: map[string]string{"type": "file", "mode": "0755", "uid": "1000", "gid": "1000"}},
		"etc":       {Path: "etc", Keywords: map[string]string{"type": "dir", "mode": "0755", "uid": "0", "gid": "0"}},
	}
}

func TestCheckFileExistencePass(t *testing.T) {
	tests := []FileExistenceTest{
		{Path: "/hello.txt", ShouldExist: true, Permissions: "-rw-r--r--", Uid: ptrInt64(0)},
		{Path: "/bin/app", ShouldExist: true, Permissions: "-rwxr-xr-x", IsExecutableBy: "owner", Uid: ptrInt64(1000)},
		{Path: "/etc", ShouldExist: true, Permissions: "drwxr-xr-x"},
		{Path: "/nope", ShouldExist: false},
	}
	if f := failures(checkFileExistence(mtreeEntries(), tests)); len(f) != 0 {
		t.Errorf("unexpected failures: %+v", f)
	}
}

func TestCheckFileExistenceFailures(t *testing.T) {
	tests := []FileExistenceTest{
		{Path: "/hello.txt", ShouldExist: false},                           // exists but shouldn't
		{Path: "/missing", ShouldExist: true},                              // missing but should
		{Path: "/hello.txt", ShouldExist: true, Permissions: "-rwxr-xr-x"}, // wrong perms
		{Path: "/hello.txt", ShouldExist: true, Uid: ptrInt64(42)},         // wrong uid
		{Path: "/hello.txt", ShouldExist: true, IsExecutableBy: "owner"},   // not executable
	}
	f := failures(checkFileExistence(mtreeEntries(), tests))
	if len(f) != 5 {
		t.Errorf("expected 5 failures, got %d: %+v", len(f), f)
	}
}

func TestCheckFileExistenceNoMtree(t *testing.T) {
	tests := []FileExistenceTest{{Path: "/x", ShouldExist: true}}
	f := failures(checkFileExistence(nil, tests))
	if len(f) != 1 {
		t.Fatalf("expected 1 failure with no mtree, got %d", len(f))
	}
}

func TestUnsupportedCategories(t *testing.T) {
	st := &StructureTest{
		CommandTests:     []CommandTest{{Name: "a"}},
		FileContentTests: []FileContentTest{{Name: "b"}},
		LicenseTests:     []LicenseTest{{Debian: true}},
	}
	if cats := unsupportedCategories(st); len(cats) != 3 {
		t.Errorf("expected 3 unsupported categories, got %v", cats)
	}
	if cats := unsupportedCategories(&StructureTest{}); len(cats) != 0 {
		t.Errorf("expected 0 unsupported categories, got %v", cats)
	}
}

func TestModeString(t *testing.T) {
	cases := []struct {
		typ  string
		mode int64
		want string
	}{
		{"file", 0o644, "-rw-r--r--"},
		{"file", 0o755, "-rwxr-xr-x"},
		{"dir", 0o755, "drwxr-xr-x"},
		{"link", 0o777, "Lrwxrwxrwx"},
		{"file", 0o4755, "urwxr-xr-x"},
	}
	for _, c := range cases {
		if got := modeString(c.typ, c.mode); got != c.want {
			t.Errorf("modeString(%q, %#o) = %q, want %q", c.typ, c.mode, got, c.want)
		}
	}
}

func TestCanonicalPath(t *testing.T) {
	cases := map[string]string{
		"/hello.txt":  "hello.txt",
		"hello.txt":   "hello.txt",
		"./hello.txt": "hello.txt",
		"/etc/app/":   "etc/app",
		"/":           ".",
		"/a/../b":     "b",
	}
	for in, want := range cases {
		if got := canonicalPath(in); got != want {
			t.Errorf("canonicalPath(%q) = %q, want %q", in, got, want)
		}
	}
}
