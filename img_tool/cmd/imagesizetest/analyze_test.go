package imagesizetest

import (
	"strconv"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/mtree"
)

func file(path string, size int64, digest string) mtree.ParsedEntry {
	kw := map[string]string{"type": "file", "size": strconv.FormatInt(size, 10), "nlink": "1"}
	if digest != "" {
		kw["sha256digest"] = digest
	}
	return mtree.ParsedEntry{Path: path, Keywords: kw}
}

// hardlinkN is one name of a hardlink group of n names.
func hardlinkN(path string, size int64, digest string, n int) mtree.ParsedEntry {
	e := file(path, size, digest)
	e.Keywords["nlink"] = strconv.Itoa(n)
	return e
}

func hardlink(path string, size int64, digest string) mtree.ParsedEntry {
	return hardlinkN(path, size, digest, 2)
}

func dir(path string) mtree.ParsedEntry {
	return mtree.ParsedEntry{Path: path, Keywords: map[string]string{"type": "dir"}}
}

func symlink(path, target string) mtree.ParsedEntry {
	return mtree.ParsedEntry{Path: path, Keywords: map[string]string{
		"type": "link", "size": strconv.Itoa(len(target)), "link": target, "nlink": "1",
	}}
}

func layer(entries ...mtree.ParsedEntry) layerInput {
	return layerInput{entries: entries}
}

func mustAnalyze(t *testing.T, layers []layerInput, checkDuplicates bool) *report {
	t.Helper()
	rep, err := analyze(layers, checkDuplicates)
	if err != nil {
		t.Fatalf("analyze: %v", err)
	}
	return rep
}

func TestNoWaste(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("etc/hello.txt", 6, "a")),
		layer(file("usr/bin/tool", 18, "b")),
	}, true)
	if rep.WastedBytes != 0 || rep.DuplicatedBytes != 0 {
		t.Errorf("wasted=%d duplicated=%d, want 0/0", rep.WastedBytes, rep.DuplicatedBytes)
	}
}

func TestOverwriteWastesEarlierBytes(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("usr/lib/libfoo.so", 100, "a")),
		layer(file("usr/lib/libfoo.so", 40, "b")),
	}, false)
	if rep.WastedBytes != 100 {
		t.Fatalf("wasted=%d, want 100", rep.WastedBytes)
	}
	if len(rep.Waste) != 1 {
		t.Fatalf("waste items=%+v, want 1", rep.Waste)
	}
	w := rep.Waste[0]
	if w.Path != "usr/lib/libfoo.so" || w.WrittenLayer != 0 || w.ShadowedLayer != 1 || w.Reason != reasonOverwritten {
		t.Errorf("waste=%+v", w)
	}
}

func TestOverwriteWithinOneLayer(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("a.txt", 10, "a"), file("a.txt", 4, "b")),
	}, false)
	if rep.WastedBytes != 10 {
		t.Errorf("wasted=%d, want 10", rep.WastedBytes)
	}
}

func TestWhiteoutDeletesFileAndDirectory(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(
			file("app/cache.bin", 50, "a"),
			file("srv/data/one", 10, "b"),
			file("srv/data/two", 20, "c"),
			file("srv/keep", 5, "d"),
		),
		layer(
			file("app/.wh.cache.bin", 0, ""),
			file("srv/.wh.data", 0, ""),
		),
	}, false)
	if rep.WastedBytes != 80 {
		t.Fatalf("wasted=%d, want 80 (50 file + 10+20 directory subtree)", rep.WastedBytes)
	}
	for _, w := range rep.Waste {
		if w.Reason != reasonDeleted || w.ShadowedLayer != 1 {
			t.Errorf("waste=%+v, want deleted by layer 1", w)
		}
	}
}

// An entry literally named ".wh." names no deletion target. Treating the empty
// remainder as a path would make it match every live file.
func TestDegenerateWhiteoutNameDeletesNothing(t *testing.T) {
	for _, name := range []string{".wh.", "etc/.wh."} {
		rep := mustAnalyze(t, []layerInput{
			layer(file("etc/a", 10, "a"), file("usr/b", 20, "b")),
			layer(file(name, 0, "")),
		}, false)
		if rep.WastedBytes != 0 {
			t.Errorf("%q: wasted=%d, want 0", name, rep.WastedBytes)
		}
	}
}

func TestOpaqueWhiteoutHidesLowerLayersOnly(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("etc/old.conf", 30, "a"), file("etc/sub/older.conf", 10, "b")),
		layer(file("etc/new.conf", 3, "c"), file("etc/.wh..wh..opq", 0, "")),
		layer(file("etc/new.conf", 3, "d")),
	}, false)
	// Both lower-layer files under etc/ are hidden (40). etc/new.conf is from
	// the opaque layer itself, so it survives the marker and is only wasted
	// when layer 3 overwrites it (3).
	if rep.WastedBytes != 43 {
		t.Fatalf("wasted=%d, want 43", rep.WastedBytes)
	}
	byPath := map[string]wasteItem{}
	for _, w := range rep.Waste {
		byPath[w.Path] = w
	}
	if w := byPath["etc/old.conf"]; w.Reason != reasonOpaque || w.ShadowedLayer != 1 {
		t.Errorf("old.conf waste=%+v", w)
	}
	if w := byPath["etc/new.conf"]; w.Reason != reasonOverwritten || w.WrittenLayer != 1 || w.ShadowedLayer != 2 {
		t.Errorf("new.conf waste=%+v", w)
	}
}

// An opaque whiteout at the image root hides every lower-layer file.
func TestOpaqueWhiteoutAtRoot(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("etc/a", 10, "a"), file("usr/lib/b", 20, "b")),
		layer(file(".wh..wh..opq", 0, ""), file("etc/a", 1, "c")),
	}, false)
	if rep.WastedBytes != 30 {
		t.Errorf("wasted=%d, want 30", rep.WastedBytes)
	}
}

func TestTypeChangeShadows(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("opt/data", 10, "a"), file("var/lib/x", 7, "b")),
		// A directory where opt/data was a file, and a file where var/lib
		// was a directory: both lower files become unreachable.
		layer(dir("opt/data"), file("var/lib", 1, "c")),
	}, false)
	if rep.WastedBytes != 17 {
		t.Errorf("wasted=%d, want 17", rep.WastedBytes)
	}
}

// Directories merge, and a sibling whose name merely shares a prefix is not
// part of the subtree.
func TestDirectoriesMergeAndPrefixSiblingsSurvive(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("a/b/c", 10, "a"), file("a/bc", 20, "b")),
		layer(dir("a"), dir("a/b"), file("a/b/d", 1, "c")),
	}, false)
	if rep.WastedBytes != 0 {
		t.Errorf("wasted=%d, want 0", rep.WastedBytes)
	}
}

func TestSymlinkCarriesNoBytes(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(symlink("etc/current", "/etc/hello.txt")),
		layer(file("etc/current", 2, "a")),
	}, false)
	if rep.WastedBytes != 0 {
		t.Errorf("wasted=%d, want 0: an overwritten symlink shipped no file bytes", rep.WastedBytes)
	}
}

// A tar whose members are absolute paths describes the same filesystem as one
// with relative members, so the two must shadow each other.
func TestAbsolutePathsAreCanonicalized(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("/usr/lib/libfoo.so", 100, "a")),
		layer(file("usr/lib/libfoo.so", 1, "b")),
	}, true)
	if rep.WastedBytes != 100 {
		t.Errorf("wasted=%d, want 100", rep.WastedBytes)
	}
	if rep.DuplicatedBytes != 0 {
		t.Errorf("duplicated=%d, want 0: the two paths are one file", rep.DuplicatedBytes)
	}
}

func TestMissingLayerFails(t *testing.T) {
	_, err := analyze([]layerInput{
		layer(file("a", 1, "a")),
		{missing: true, name: "@debian_base"},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "incomplete layer metadata") || !strings.Contains(err.Error(), "layer 2 of 2") {
		t.Fatalf("err=%v, want incomplete layer metadata for layer 2 of 2", err)
	}
	// The error has to name the layer and the remedy, or it is unactionable.
	for _, want := range []string{"@debian_base", "layer_handling = \"eager\""} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err=%v, want it to mention %q", err, want)
		}
	}
}

func TestMissingSizeFails(t *testing.T) {
	noSize := mtree.ParsedEntry{Path: "a", Keywords: map[string]string{"type": "file"}}
	_, err := analyze([]layerInput{layer(noSize)}, false)
	if err == nil || !strings.Contains(err.Error(), "incomplete layer metadata") {
		t.Errorf("err=%v, want incomplete layer metadata", err)
	}
}

func TestDuplicatesCountExtraCopiesOnce(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("lib/a.so", 100, "same"), file("lib/b.so", 100, "same")),
		layer(file("opt/c.so", 100, "same"), file("opt/unique", 5, "other")),
	}, true)
	if rep.DuplicatedBytes != 200 {
		t.Fatalf("duplicated=%d, want 200 (2 extra copies of 100)", rep.DuplicatedBytes)
	}
	if len(rep.Duplicates) != 1 || len(rep.Duplicates[0].Paths) != 3 {
		t.Errorf("duplicates=%+v", rep.Duplicates)
	}
}

func TestDuplicatesCountHardlinkGroupAsOneCopy(t *testing.T) {
	// One hardlink group (two names, one shipped copy) plus a plain file with
	// the same content: the image really carries the content twice.
	rep := mustAnalyze(t, []layerInput{
		layer(hardlink("bin/x", 80, "h"), hardlink("bin/y", 80, "h")),
		layer(file("opt/z", 80, "h")),
	}, true)
	if rep.DuplicatedBytes != 80 {
		t.Fatalf("duplicated=%d, want 80 (two shipped copies, one extra)", rep.DuplicatedBytes)
	}
	if len(rep.Duplicates) != 1 || len(rep.Duplicates[0].Paths) != 2 {
		t.Fatalf("duplicates=%+v, want one group of two representative paths", rep.Duplicates)
	}
	if got := strings.Join(rep.Duplicates[0].Paths, ","); got != "bin/x,opt/z" {
		t.Errorf("paths=%s, want bin/x,opt/z (one representative per shipped copy)", got)
	}
}

func TestDuplicatesIgnoreShadowedCopies(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		// One shipped copy, two names. Plus a copy that layer 2 overwrites,
		// which is waste rather than a live duplicate.
		layer(hardlink("bin/x", 80, "h"), hardlink("bin/y", 80, "h"), file("bin/z", 80, "h")),
		layer(file("bin/z", 1, "z2")),
	}, true)
	if rep.DuplicatedBytes != 0 {
		t.Errorf("duplicated=%d, want 0", rep.DuplicatedBytes)
	}
	if rep.WastedBytes != 80 {
		t.Errorf("wasted=%d, want 80 (the overwritten bin/z copy)", rep.WastedBytes)
	}
}

func TestHardlinkBytesWastedOnceWhenAllNamesShadowed(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(hardlink("bin/x", 80, "h"), hardlink("bin/y", 80, "h")),
		layer(file("bin/x", 1, "n")),
	}, false)
	if rep.WastedBytes != 0 {
		t.Fatalf("wasted=%d, want 0: content still reachable as bin/y", rep.WastedBytes)
	}
	rep = mustAnalyze(t, []layerInput{
		layer(hardlink("bin/x", 80, "h"), hardlink("bin/y", 80, "h")),
		layer(file("bin/x", 1, "n"), file("bin/.wh.y", 0, "")),
	}, false)
	if rep.WastedBytes != 80 {
		t.Errorf("wasted=%d, want 80 (one shipped copy, counted once)", rep.WastedBytes)
	}
	if len(rep.Waste) != 1 || rep.Waste[0].Path != "bin/x" {
		t.Errorf("waste=%+v, want one item naming bin/x", rep.Waste)
	}
}

// Two independent hardlink groups of the same content in one layer ship two
// copies: nlink says how many names share a copy, so 4 names with nlink=2 are
// 2 copies, not 1.
func TestIndependentHardlinkGroupsWithEqualContent(t *testing.T) {
	stack := []layerInput{
		layer(
			hardlink("g1/a", 80, "h"), hardlink("g1/b", 80, "h"),
			hardlink("g2/a", 80, "h"), hardlink("g2/b", 80, "h"),
		),
		layer(file("g1/.wh.a", 0, ""), file("g1/.wh.b", 0, ""),
			file("g2/.wh.a", 0, ""), file("g2/.wh.b", 0, "")),
	}
	rep := mustAnalyze(t, stack, false)
	if rep.WastedBytes != 160 {
		t.Errorf("wasted=%d, want 160 (two shipped copies)", rep.WastedBytes)
	}

	// Live, the same four names are two duplicate copies.
	rep = mustAnalyze(t, stack[:1], true)
	if rep.DuplicatedBytes != 80 {
		t.Errorf("duplicated=%d, want 80 (one extra copy)", rep.DuplicatedBytes)
	}
}

// A hardlink farm (one copy, many names) is free of duplicates and costs its
// single copy once.
func TestHardlinkFarmCostsOneCopy(t *testing.T) {
	var names []mtree.ParsedEntry
	for i := 0; i < 50; i++ {
		names = append(names, hardlinkN("bin/applet"+strconv.Itoa(i), 1000, "busybox", 50))
	}
	rep := mustAnalyze(t, []layerInput{layer(names...)}, true)
	if rep.DuplicatedBytes != 0 {
		t.Errorf("duplicated=%d, want 0", rep.DuplicatedBytes)
	}
	rep = mustAnalyze(t, []layerInput{layer(names...), layer(file(".wh..wh..opq", 0, ""))}, false)
	if rep.WastedBytes != 1000 {
		t.Errorf("wasted=%d, want 1000: an opaque marker at the root hides the whole lower filesystem, "+
			"and the farm's single shipped copy is counted once", rep.WastedBytes)
	}
	rep = mustAnalyze(t, []layerInput{layer(names...), layer(file("bin/.wh..wh..opq", 0, ""))}, false)
	if rep.WastedBytes != 1000 {
		t.Errorf("wasted=%d, want 1000 (one shipped copy)", rep.WastedBytes)
	}
}

func TestDuplicatesWithoutDigestAreUnmeasuredNotZero(t *testing.T) {
	layers := []layerInput{layer(file("a", 10, ""), file("b", 10, ""))}
	rep, err := analyze(layers, false)
	if err != nil {
		t.Fatalf("without a duplicate budget a missing digest is fine: %v", err)
	}
	if rep.DuplicatesMeasured {
		t.Errorf("DuplicatesMeasured=true, want false: without digests nothing was measured")
	}
	_, err = analyze(layers, true)
	if err == nil || !strings.Contains(err.Error(), "incomplete layer metadata") {
		t.Errorf("err=%v, want incomplete layer metadata", err)
	}
}

func TestDuplicatesMeasuredWhenDigestsPresent(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{layer(file("a", 10, "a"))}, false)
	if !rep.DuplicatesMeasured {
		t.Errorf("DuplicatesMeasured=false, want true even without a budget")
	}
}

func TestEmptyFilesAreFree(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("a", 0, "e"), file("b", 0, "e"), file("c", 0, "e")),
		layer(file("a", 0, "e2")),
	}, true)
	if rep.WastedBytes != 0 || rep.DuplicatedBytes != 0 {
		t.Errorf("wasted=%d duplicated=%d, want 0/0", rep.WastedBytes, rep.DuplicatedBytes)
	}
}

func TestWasteSortedLargestFirst(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(file("small", 1, "a"), file("big", 100, "b"), file("mid", 50, "c")),
		layer(file("small", 1, "x"), file("big", 1, "y"), file("mid", 1, "z")),
	}, false)
	var paths []string
	for _, w := range rep.Waste {
		paths = append(paths, w.Path)
	}
	if strings.Join(paths, ",") != "big,mid,small" {
		t.Errorf("order=%v, want big,mid,small", paths)
	}
}

// The replay has to stay linear-ish in the number of entries: an image with
// tens of thousands of files is the normal case for this rule, and a
// quadratic scan would blow the test timeout rather than report a budget.
func TestLargeImageIsNotQuadratic(t *testing.T) {
	build := func(n int) []layerInput {
		var base, top []mtree.ParsedEntry
		for i := 0; i < n; i++ {
			p := "usr/share/pkg" + strconv.Itoa(i%97) + "/file" + strconv.Itoa(i)
			base = append(base, file(p, 10, "d"+strconv.Itoa(i)))
			if i%10 == 0 {
				top = append(top, file(p, 1, "n"+strconv.Itoa(i)))
			}
		}
		return []layerInput{layer(base...), layer(top...)}
	}
	// 40k entries: >1.5 min with a per-entry scan of the live set, well under a
	// second without one.
	rep := mustAnalyze(t, build(40000), true)
	if rep.WastedBytes != 40000 {
		t.Errorf("wasted=%d, want 40000 (4000 shadowed files of 10 bytes)", rep.WastedBytes)
	}
}
