package layer

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDedupeSymlinks(t *testing.T) {
	tests := []struct {
		name string
		in   symlinks
		want symlinks
	}{
		{
			name: "empty",
			in:   nil,
			want: nil,
		},
		{
			// The shape a shared runfiles tree produces: "_main" is named by the
			// group's files and again by a non-root symlink, which resolves under
			// "_main" whatever its own first segment says.
			name: "exact repeat collapses",
			in: symlinks{
				{LinkName: "app/bin/launcher.runfiles/_main", Target: "../../../.shared_runfiles/_main"},
				{LinkName: "app/bin/launcher.runfiles/external_tool", Target: "../../../.shared_runfiles/external_tool"},
				{LinkName: "app/bin/launcher.runfiles/_main", Target: "../../../.shared_runfiles/_main"},
			},
			want: symlinks{
				{LinkName: "app/bin/launcher.runfiles/_main", Target: "../../../.shared_runfiles/_main"},
				{LinkName: "app/bin/launcher.runfiles/external_tool", Target: "../../../.shared_runfiles/external_tool"},
			},
		},
		{
			// Same name, different target: a conflict rather than a repeat, so
			// both survive and the layer shows it.
			name: "same name different target survives",
			in: symlinks{
				{LinkName: "link", Target: "first"},
				{LinkName: "link", Target: "second"},
			},
			want: symlinks{
				{LinkName: "link", Target: "first"},
				{LinkName: "link", Target: "second"},
			},
		},
		{
			name: "order of the survivors is kept",
			in: symlinks{
				{LinkName: "c", Target: "t"},
				{LinkName: "a", Target: "t"},
				{LinkName: "c", Target: "t"},
				{LinkName: "b", Target: "t"},
			},
			want: symlinks{
				{LinkName: "c", Target: "t"},
				{LinkName: "a", Target: "t"},
				{LinkName: "b", Target: "t"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupeSymlinks(tc.in)
			if !slices.Equal(got, tc.want) {
				t.Errorf("dedupeSymlinks() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadParamFileDedupDirectives(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		"a\x00f" + src,
		"\x00dedup=content",
		"b\x00f" + src,
		"c\x00f" + src,
		"\x00dedup=exact",
		"d\x00f" + src,
	}
	paramFile := filepath.Join(dir, "params")
	if err := os.WriteFile(paramFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := readParamFile(paramFile)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"a": false, "b": true, "c": true, "d": false}
	if len(files) != len(want) {
		t.Fatalf("got %d files, want %d", len(files), len(want))
	}
	for _, f := range files {
		if f.DedupByContentOK != want[f.PathInImage] {
			t.Errorf("%s: DedupByContentOK = %v, want %v", f.PathInImage, f.DedupByContentOK, want[f.PathInImage])
		}
	}

	for _, bad := range []string{"\x00dedup=bogus", "\x00nope=1", "\x00dedup"} {
		if err := os.WriteFile(paramFile, []byte(bad+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readParamFile(paramFile); err == nil {
			t.Errorf("readParamFile(%q) succeeded, want error", bad)
		}
	}
}
