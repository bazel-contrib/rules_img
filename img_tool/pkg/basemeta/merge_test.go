package basemeta

import (
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// paths returns the entry paths of a merge result, for comparison.
func paths(entries []*baselayer.BaseEntry) []string {
	out := make([]string, len(entries))
	for i, entry := range entries {
		out[i] = entry.GetPath()
	}
	return out
}

// TestMergeSortsParentsFirst checks that the merged order is usable as tar
// order: a directory always precedes what lives inside it.
func TestMergeSortsParentsFirst(t *testing.T) {
	merged, err := Merge([][]*baselayer.BaseEntry{{
		File("/usr/lib/libc.so.6", 0o755, nil),
		Dir("/usr", 0o755),
		Dir("/usr/lib", 0o755),
		Dir("/etc", 0o755),
	}}, DefaultMergeOptions())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}

	want := []string{"etc", "usr", "usr/lib", "usr/lib/libc.so.6"}
	got := paths(merged)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("merged order = %v, want %v", got, want)
	}
}

// TestMergeLastStreamWins checks the override rule that lets a rule closer to
// the layer replace what it depends on.
func TestMergeLastStreamWins(t *testing.T) {
	first := WithProducer([]*baselayer.BaseEntry{Dir("/tmp", 0o755)}, "//base:skeleton")
	second := WithProducer([]*baselayer.BaseEntry{Dir("/tmp", 0o1777)}, "//base:override")

	merged, err := Merge([][]*baselayer.BaseEntry{first, second}, DefaultMergeOptions())
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("merged %d entries, want 1", len(merged))
	}
	if merged[0].GetMode() != 0o1777 {
		t.Errorf("mode = %o, want %o (the later stream should win)", merged[0].GetMode(), 0o1777)
	}
}

// TestMergeRejectsTypeConflict checks that a directory shadowing a symlink
// fails, and that the error names both rules so the conflict is actionable.
func TestMergeRejectsTypeConflict(t *testing.T) {
	first := WithProducer([]*baselayer.BaseEntry{Symlink("/lib", "usr/lib")}, "//base:skeleton")
	second := WithProducer([]*baselayer.BaseEntry{Dir("/lib", 0o755)}, "//base:libs")

	_, err := Merge([][]*baselayer.BaseEntry{first, second}, DefaultMergeOptions())
	if err == nil {
		t.Fatal("Merge accepted a symlink and a directory at the same path")
	}
	for _, want := range []string{"//base:skeleton", "//base:libs", "symlink", "directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestMergeRejectsFileUnderSymlink checks the case the type-conflict check
// cannot see: two entries that are individually fine but describe a file living
// inside a symlink. In practice this is a usr_merged mismatch between the
// skeleton and whatever places libraries.
func TestMergeRejectsFileUnderSymlink(t *testing.T) {
	skeleton := WithProducer([]*baselayer.BaseEntry{Symlink("/lib", "usr/lib")}, "//base:skeleton")
	libs := WithProducer([]*baselayer.BaseEntry{File("/lib/libc.so.6", 0o755, nil)}, "//base:libs")

	_, err := Merge([][]*baselayer.BaseEntry{skeleton, libs}, DefaultMergeOptions())
	if err == nil {
		t.Fatal("Merge accepted a file placed underneath a symlink")
	}
	for _, want := range []string{"//base:skeleton", "//base:libs", "lib/libc.so.6", "symlink"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestMergeCreatesParentDirectories checks that missing parents are synthesized
// when asked for, and only then.
func TestMergeCreatesParentDirectories(t *testing.T) {
	stream := []*baselayer.BaseEntry{File("/var/lib/app/state.json", 0o644, nil)}

	without, err := Merge([][]*baselayer.BaseEntry{stream}, MergeOptions{})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(without) != 1 {
		t.Errorf("merged %v, want just the file when parent creation is off", paths(without))
	}

	with, err := Merge([][]*baselayer.BaseEntry{stream}, MergeOptions{
		CreateParentDirectories: true,
		ParentDirectoryMode:     0o755,
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	want := []string{"var", "var/lib", "var/lib/app", "var/lib/app/state.json"}
	if got := paths(with); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("merged = %v, want %v", got, want)
	}
	for _, entry := range with[:3] {
		if entry.GetType() != baselayer.EntryType_ENTRY_TYPE_DIRECTORY {
			t.Errorf("%s: type = %v, want directory", entry.GetPath(), entry.GetType())
		}
	}
}

// TestMergeKeepsDescribedParents checks that a parent an input already
// describes is not replaced by a synthesized default. Getting this wrong would
// silently reset /tmp to 0755.
func TestMergeKeepsDescribedParents(t *testing.T) {
	merged, err := Merge([][]*baselayer.BaseEntry{{
		Dir("/tmp", 0o1777),
		File("/tmp/seed", 0o600, nil),
	}}, MergeOptions{CreateParentDirectories: true, ParentDirectoryMode: 0o755})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if merged[0].GetPath() != "tmp" {
		t.Fatalf("first entry = %q, want tmp", merged[0].GetPath())
	}
	if merged[0].GetMode() != 0o1777 {
		t.Errorf("tmp mode = %o, want %o", merged[0].GetMode(), 0o1777)
	}
}

// TestMergeRejectsEmptyPath makes sure an entry with no path (or one that
// normalizes to nothing, like "/") is caught rather than written as a nameless
// tar entry.
func TestMergeRejectsEmptyPath(t *testing.T) {
	_, err := Merge([][]*baselayer.BaseEntry{
		WithProducer([]*baselayer.BaseEntry{Dir("/", 0o755)}, "//base:broken"),
	}, DefaultMergeOptions())
	if err == nil {
		t.Fatal("Merge accepted an entry with an empty path")
	}
	if !strings.Contains(err.Error(), "//base:broken") {
		t.Errorf("error %q does not name the producing rule", err)
	}
}
