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

func hardlink(path string, size int64, digest string) mtree.ParsedEntry {
	e := file(path, size, digest)
	e.Keywords["nlink"] = "2"
	return e
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
	if w.Path != "usr/lib/libfoo.so" || w.WrittenLayer != 0 || w.ShadowedLayer != 1 || w.Reason != "overwritten" {
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
		if w.Reason != "deleted" || w.ShadowedLayer != 1 {
			t.Errorf("waste=%+v, want deleted by layer 1", w)
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
	if w := byPath["etc/old.conf"]; w.Reason != "hidden by opaque whiteout" || w.ShadowedLayer != 1 {
		t.Errorf("old.conf waste=%+v", w)
	}
	if w := byPath["etc/new.conf"]; w.Reason != "overwritten" || w.WrittenLayer != 1 || w.ShadowedLayer != 2 {
		t.Errorf("new.conf waste=%+v", w)
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

func TestSymlinkCarriesNoBytes(t *testing.T) {
	rep := mustAnalyze(t, []layerInput{
		layer(symlink("etc/current", "/etc/hello.txt")),
		layer(file("etc/current", 2, "a")),
	}, false)
	if rep.WastedBytes != 0 {
		t.Errorf("wasted=%d, want 0: an overwritten symlink shipped no file bytes", rep.WastedBytes)
	}
}

func TestMissingLayerFails(t *testing.T) {
	_, err := analyze([]layerInput{
		layer(file("a", 1, "a")),
		{missing: true},
	}, false)
	if err == nil || !strings.Contains(err.Error(), "incomplete layer metadata") || !strings.Contains(err.Error(), "layer 2 of 2") {
		t.Errorf("err=%v, want incomplete layer metadata for layer 2 of 2", err)
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

func TestDuplicatesIgnoreHardlinksAndShadowedCopies(t *testing.T) {
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
}

func TestDuplicatesRequireDigest(t *testing.T) {
	layers := []layerInput{layer(file("a", 10, ""))}
	if _, err := analyze(layers, false); err != nil {
		t.Errorf("without a duplicate budget a missing digest is fine: %v", err)
	}
	_, err := analyze(layers, true)
	if err == nil || !strings.Contains(err.Error(), "incomplete layer metadata") {
		t.Errorf("err=%v, want incomplete layer metadata", err)
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
