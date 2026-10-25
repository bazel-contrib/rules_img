package imagesizetest

import (
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/mtree"
)

func writeMetadata(t *testing.T, desc api.Descriptor) string {
	t.Helper()
	data, err := json.Marshal(desc)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "metadata.json")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A pulled layer carries the metadata-only steps that preceded it as
// empty_layer history entries. Those shipped no bytes, so attributing the
// layer to one of them would point at the wrong build step.
func TestLayerNameSkipsEmptyLayerHistory(t *testing.T) {
	p := writeMetadata(t, api.Descriptor{History: []api.History{
		{CreatedBy: "/bin/sh -c #(nop)  ENV PATH=/usr/local/bin", EmptyLayer: true},
		{CreatedBy: "/bin/sh -c #(nop) WORKDIR /app", EmptyLayer: true},
		{CreatedBy: "/bin/sh -c apt-get install -y python3"},
	}})
	name, err := layerName(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/bin/sh -c apt-get install -y python3"; name != want {
		t.Errorf("layerName = %q, want %q", name, want)
	}
}

func TestLayerNameStripsBazelBuildPrefix(t *testing.T) {
	p := writeMetadata(t, api.Descriptor{History: []api.History{
		{CreatedBy: api.BazelBuildHistoryPrefix + "//app:deps"},
	}})
	name, err := layerName(p)
	if err != nil {
		t.Fatal(err)
	}
	if name != "//app:deps" {
		t.Errorf("layerName = %q, want //app:deps", name)
	}
}

// When every entry is an empty-layer one (a hand-written or unusual config),
// fall back to naming something rather than nothing.
func TestLayerNameFallsBackToEmptyLayerEntries(t *testing.T) {
	p := writeMetadata(t, api.Descriptor{History: []api.History{
		{CreatedBy: api.MissingHistoryMarker, EmptyLayer: true},
		{CreatedBy: "COPY . /app", EmptyLayer: true},
	}})
	name, err := layerName(p)
	if err != nil {
		t.Fatal(err)
	}
	if name != "COPY . /app" {
		t.Errorf("layerName = %q, want COPY . /app", name)
	}
}

// The replay indexes live paths by directory so a whiteout or a type change can
// find the subtree it hides without scanning every live file. This checks the
// index against a brute-force scan over randomized insert/shadow sequences --
// the index is the whole reason the replay is not quadratic, and a stale entry
// in it would silently miscount bytes.
func TestDescendantsMatchesBruteForce(t *testing.T) {
	dirs := []string{"", "a", "a/b", "a/b/c", "a/bc", "x", "x/y"}
	paths := []string{
		"a/b/c/one", "a/b/c/two", "a/b/d", "a/bc", "a/bcd/e",
		"x/y/z", "x/w", "top", "a/b/c", "a",
	}
	rng := rand.New(rand.NewSource(7))

	bruteForce := func(live map[string]*fileGroup, dir string) []string {
		var out []string
		for p := range live {
			if dir == "" {
				out = append(out, p)
				continue
			}
			if strings.HasPrefix(p, dir+"/") {
				out = append(out, p)
			}
		}
		sort.Strings(out)
		return out
	}

	for iteration := 0; iteration < 2000; iteration++ {
		r := &replay{
			live:     map[string]*fileGroup{},
			children: map[string]map[string]bool{},
			rep:      &report{},
		}
		for step := 0; step < 12; step++ {
			p := paths[rng.Intn(len(paths))]
			if rng.Intn(3) == 0 {
				r.shadow(p, 1, reasonDeleted)
			} else if _, live := r.live[p]; !live {
				// Only insert where no live path is a prefix of another, the
				// invariant applyEntry maintains.
				conflict := false
				for q := range r.live {
					if strings.HasPrefix(q, p+"/") || strings.HasPrefix(p, q+"/") {
						conflict = true
						break
					}
				}
				if !conflict {
					r.insert(p, &fileGroup{size: 1, names: 1, live: 1, want: 1, minPath: p})
				}
			}
			for _, dir := range dirs {
				got := r.descendants(dir)
				want := bruteForce(r.live, dir)
				if strings.Join(got, ",") != strings.Join(want, ",") {
					t.Fatalf("iteration %d step %d: descendants(%q) = %v, want %v (live=%v)",
						iteration, step, dir, got, want, sortedLive(r))
				}
			}
		}
		// Every index entry must be reachable from a live path: a leftover one
		// would keep a dead subtree alive for later lookups.
		for dir, names := range r.children {
			if len(names) == 0 {
				t.Fatalf("iteration %d: empty index entry for %q", iteration, dir)
			}
			if dir != "" {
				if _, ok := r.live[dir]; ok {
					t.Fatalf("iteration %d: %q is both a live file and a directory", iteration, dir)
				}
			}
			if len(r.descendants(dir)) == 0 {
				t.Fatalf("iteration %d: index entry %q has no live descendants", iteration, dir)
			}
		}
	}
}

func sortedLive(r *replay) []string {
	var out []string
	for p := range r.live {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Shadowing a path must leave the index usable for the paths that remain, so
// the byte accounting over a wide, deeply nested image has to agree with a
// straight sum over the entries that get shadowed.
func TestShadowAccountingOverWideImage(t *testing.T) {
	const n = 300
	var baseEntries, topEntries []mtree.ParsedEntry
	want := int64(0)
	for i := 0; i < n; i++ {
		d := "d" + strconv.Itoa(i%7)
		p := d + "/f" + strconv.Itoa(i)
		baseEntries = append(baseEntries, file(p, int64(i+1), "h"+strconv.Itoa(i)))
		switch {
		case i%3 == 0:
			topEntries = append(topEntries, file(p, 1, "n"+strconv.Itoa(i)))
			want += int64(i + 1)
		case i%5 == 0:
			topEntries = append(topEntries, file(d+"/.wh.f"+strconv.Itoa(i), 0, ""))
			want += int64(i + 1)
		}
	}
	rep := mustAnalyze(t, []layerInput{layer(baseEntries...), layer(topEntries...)}, true)
	if rep.WastedBytes != want {
		t.Errorf("wasted=%d, want %d", rep.WastedBytes, want)
	}
	if !rep.DuplicatesMeasured || rep.DuplicatedBytes != 0 {
		t.Errorf("duplicated=%d measured=%v, want 0/true: every file has unique content",
			rep.DuplicatedBytes, rep.DuplicatesMeasured)
	}
}
