package imagesizetest

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/mtree"
)

// layerInput is one layer's parsed mtree entries, in mtree (tar) order. A
// layer with missing=true contributed no mtree (e.g. a shallow base-image
// layer whose blob is never fetched); analyzing such an image would silently
// ignore part of the filesystem, so it is an error.
type layerInput struct {
	// name identifies the layer for reports (typically the producing target
	// label, from the layer's history created_by), or "" when unknown.
	name    string
	missing bool
	entries []mtree.ParsedEntry
}

// shadowReason is why a shipped file copy became unreachable.
type shadowReason int

const (
	reasonOverwritten shadowReason = iota
	reasonDeleted
	reasonOpaque
)

// String names the reason on its own ("overwritten").
func (r shadowReason) String() string {
	switch r {
	case reasonDeleted:
		return "deleted"
	case reasonOpaque:
		return "hidden by opaque whiteout"
	default:
		return "overwritten"
	}
}

// Phrase names the reason followed by the preposition that introduces the
// shadowing layer ("overwritten by layer 3", "hidden by opaque whiteout in
// layer 3").
func (r shadowReason) Phrase() string {
	if r == reasonOpaque {
		return r.String() + " in"
	}
	return r.String() + " by"
}

// wasteItem is one shipped file copy that no process can see in the final
// filesystem because a later layer shadowed it.
type wasteItem struct {
	Path          string
	Size          int64
	WrittenLayer  int // 0-based layer index that shipped the bytes
	ShadowedLayer int // 0-based layer index that made them unreachable
	Reason        shadowReason
}

// duplicateGroup is identical file content live at more than one path.
type duplicateGroup struct {
	Size  int64 // size of one copy
	Paths []string
}

// ExtraBytes is the cost of the copies beyond the first.
func (g duplicateGroup) ExtraBytes() int64 {
	return g.Size * int64(len(g.Paths)-1)
}

type report struct {
	WastedBytes int64
	Waste       []wasteItem // largest first
	// DuplicatesMeasured is false when the mtrees lacked the sha256digest
	// needed to group content, so DuplicatedBytes says nothing about the image.
	DuplicatesMeasured bool
	DuplicatedBytes    int64
	Duplicates         []duplicateGroup // largest extra cost first
}

// fileGroup is one shipped copy of file content in one layer: either a single
// regular file, or a hardlink group whose names all share that copy. The
// content costs `size` bytes exactly once, and is only wasted once every name
// pointing at it has been shadowed.
type fileGroup struct {
	layer int
	size  int64
	// digest is the content sha256, or "" when the mtree did not record one.
	digest string
	// names is how many paths were placed in this group, live is how many of
	// them are still visible.
	names int
	live  int
	// want is the number of names the group expects (its nlink); 1 for a plain
	// file.
	want int
	// minPath is the lexicographically smallest name ever placed in the group,
	// used to attribute the wasted bytes.
	minPath string
}

// analyze replays the per-layer mtrees in layer order and computes the wasted
// bytes (shipped but shadowed by an overwrite, whiteout, or opaque whiteout in
// a later layer) and the duplicated bytes (identical sha256digest content live
// at multiple paths, each hardlink group counting as the single copy it
// ships). Only regular files carry bytes; sizes are the uncompressed tar entry
// sizes recorded in the mtree.
//
// duplicatesBudgeted makes missing content digests fatal: without them
// duplicated bytes cannot be computed at all, which only matters when a budget
// asked for them.
func analyze(layers []layerInput, duplicatesBudgeted bool) (*report, error) {
	for i, layer := range layers {
		if layer.missing {
			desc := fmt.Sprintf("layer %d of %d", i+1, len(layers))
			if layer.name != "" {
				desc += " (" + layer.name + ")"
			}
			return nil, fmt.Errorf("incomplete layer metadata: %s has no mtree, so wasted bytes "+
				"cannot be computed for the whole image. A base image pulled with the default "+
				"layer_handling = \"shallow\" never fetches its layer blobs; re-pull it with "+
				"layer_handling = \"eager\" to make its layers measurable", desc)
		}
	}

	r := &replay{
		live:     map[string]*fileGroup{},
		children: map[string]map[string]bool{},
		rep:      &report{},
	}
	for li, layer := range layers {
		if err := r.applyLayer(li, layer); err != nil {
			return nil, err
		}
	}

	if err := r.collectDuplicates(duplicatesBudgeted); err != nil {
		return nil, err
	}

	rep := r.rep
	sort.Slice(rep.Waste, func(i, j int) bool {
		a, b := rep.Waste[i], rep.Waste[j]
		if a.Size != b.Size {
			return a.Size > b.Size
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.WrittenLayer < b.WrittenLayer
	})
	return rep, nil
}

// replay is the filesystem as seen after the layers applied so far: live maps
// each visible regular file's path to the shipped copy it exposes, and children
// indexes those paths by directory so a whiteout or a type change can find the
// subtree it hides without scanning the whole filesystem.
//
// Every live path is a leaf of the children index: a path that becomes a
// directory shadows the file that was there (see applyEntry), so no live path
// is ever a prefix of another.
type replay struct {
	live     map[string]*fileGroup
	children map[string]map[string]bool
	rep      *report
}

func (r *replay) insert(p string, g *fileGroup) {
	r.live[p] = g
	for cur := p; ; {
		parent := parentDir(cur)
		set := r.children[parent]
		if set == nil {
			set = map[string]bool{}
			r.children[parent] = set
		}
		name := path.Base(cur)
		if set[name] {
			return // the rest of the chain is already indexed
		}
		set[name] = true
		if parent == "" {
			return
		}
		cur = parent
	}
}

func (r *replay) unindex(p string) {
	for cur := p; ; {
		parent := parentDir(cur)
		set := r.children[parent]
		if set == nil {
			return
		}
		name := path.Base(cur)
		if r.children[cur] != nil {
			return // still has live descendants
		}
		delete(set, name)
		if len(set) > 0 {
			return
		}
		delete(r.children, parent)
		if parent == "" {
			return
		}
		cur = parent
	}
}

// shadow removes the file visible at p and, once every name exposing its
// shipped copy is gone, books that copy as wasted.
func (r *replay) shadow(p string, byLayer int, reason shadowReason) {
	g := r.live[p]
	if g == nil {
		return
	}
	delete(r.live, p)
	r.unindex(p)
	g.live--
	if g.live > 0 || g.size == 0 {
		return
	}
	r.rep.WastedBytes += g.size
	r.rep.Waste = append(r.rep.Waste, wasteItem{
		Path:          g.minPath,
		Size:          g.size,
		WrittenLayer:  g.layer,
		ShadowedLayer: byLayer,
		Reason:        reason,
	})
}

// descendants returns the live paths strictly below dir, in path order. The
// root ("") contains every path.
func (r *replay) descendants(dir string) []string {
	var out []string
	var walk func(d string)
	walk = func(d string) {
		names := make([]string, 0, len(r.children[d]))
		for name := range r.children[d] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := name
			if d != "" {
				child = d + "/" + name
			}
			if _, ok := r.live[child]; ok {
				out = append(out, child)
			}
			if r.children[child] != nil {
				walk(child)
			}
		}
	}
	if dir != "" && r.children[dir] == nil {
		return nil
	}
	walk(dir)
	return out
}

// shadowSubtree shadows every live path strictly below dir for which keep
// reports false (keep may be nil to shadow all of them).
func (r *replay) shadowSubtree(dir string, byLayer int, reason shadowReason, survives func(*fileGroup) bool) {
	for _, q := range r.descendants(dir) {
		if survives != nil && survives(r.live[q]) {
			continue
		}
		r.shadow(q, byLayer, reason)
	}
}

// shadowAncestors shadows a lower layer's file that p now turns into a
// directory (a file at "usr/lib" with "usr/lib/libfoo.so" coming in).
func (r *replay) shadowAncestors(p string, byLayer int, reason shadowReason) {
	for cur := parentDir(p); cur != ""; cur = parentDir(cur) {
		r.shadow(cur, byLayer, reason)
	}
}

func (r *replay) applyLayer(li int, layer layerInput) error {
	// Hardlink groups still accepting names in this layer, keyed by content
	// digest. The mtree renders every name of a hardlink group as a full copy
	// of the target with the group's nlink and no link target (see
	// pkg/mtree.materialize), so the only way to tell two same-content groups
	// in one layer apart is to count: nlink names make one shipped copy.
	// Which name lands in which group is therefore arbitrary when groups are
	// interleaved; the number of shipped copies is not.
	open := map[string]*fileGroup{}
	for _, e := range layer.entries {
		if err := r.applyEntry(li, e, open); err != nil {
			return err
		}
	}
	return nil
}

func (r *replay) applyEntry(li int, e mtree.ParsedEntry, open map[string]*fileGroup) error {
	// A tar member path may be absolute ("/usr/lib/x"); the mtree keeps the
	// leading slash, so strip it to compare against relative lower-layer paths.
	p := strings.TrimPrefix(e.Path, "/")
	if p == "." || p == "" {
		return nil
	}
	base := path.Base(p)
	parent := parentDir(p)
	switch {
	case base == mtree.WhiteoutOpaque:
		// Hide everything lower layers put in this directory. Entries from
		// this layer survive.
		r.shadowSubtree(parent, li, reasonOpaque, func(g *fileGroup) bool { return g.layer >= li })
		return nil
	case strings.HasPrefix(base, mtree.WhiteoutPrefix):
		name := strings.TrimPrefix(base, mtree.WhiteoutPrefix)
		if name == "" {
			// Not a whiteout: an entry literally named ".wh." names no
			// target. Runtimes reject it; it deletes nothing.
			return nil
		}
		target := name
		if parent != "" {
			target = parent + "/" + name
		}
		r.shadow(target, li, reasonDeleted)
		r.shadowSubtree(target, li, reasonDeleted, nil)
		return nil
	}

	typ := e.Keywords["type"]
	// A file (or other non-directory) replaces whatever a lower layer had at
	// this path and below it, and anything below a lower layer's file replaces
	// that file with a directory. A directory only shadows a file at its own
	// path; directories merge.
	r.shadow(p, li, reasonOverwritten)
	if typ != "dir" {
		r.shadowSubtree(p, li, reasonOverwritten, nil)
	}
	r.shadowAncestors(p, li, reasonOverwritten)

	if typ != "file" {
		return nil
	}
	size, err := fileSize(e, li)
	if err != nil {
		return err
	}
	nlink := parseInt(e.Keywords["nlink"], 1)
	digest := e.Keywords["sha256digest"]
	r.insert(p, groupFor(open, li, p, size, nlink, digest))
	return nil
}

// groupFor returns the shipped copy the path p exposes: a fresh single-name
// group for a plain file, or the hardlink group of this layer that still has
// room for another name.
func groupFor(open map[string]*fileGroup, li int, p string, size, nlink int64, digest string) *fileGroup {
	if nlink <= 1 || digest == "" {
		return &fileGroup{layer: li, size: size, digest: digest, names: 1, live: 1, want: 1, minPath: p}
	}
	g := open[digest]
	if g == nil {
		g = &fileGroup{layer: li, size: size, digest: digest, want: int(nlink), minPath: p}
		open[digest] = g
	}
	g.names++
	g.live++
	if p < g.minPath {
		g.minPath = p
	}
	if g.names >= g.want {
		delete(open, digest) // full: the next same-content name starts a new copy
	}
	return g
}

// collectDuplicates groups the live shipped copies by content digest. Each
// hardlink group counts once however many names it has, and empty files cost
// nothing, so neither shows up as a duplicate.
func (r *replay) collectDuplicates(budgeted bool) error {
	// One representative (lexicographically smallest) live path per shipped
	// copy.
	reps := map[*fileGroup]string{}
	for p, g := range r.live {
		if g.size == 0 {
			continue
		}
		if cur, ok := reps[g]; !ok || p < cur {
			reps[g] = p
		}
	}

	byDigest := map[string]*duplicateGroup{}
	var undigested string
	var undigestedLayer int
	for g, p := range reps {
		if g.digest == "" {
			if undigested == "" || p < undigested {
				undigested, undigestedLayer = p, g.layer
			}
			continue
		}
		group := byDigest[g.digest]
		if group == nil {
			group = &duplicateGroup{Size: g.size}
			byDigest[g.digest] = group
		}
		group.Paths = append(group.Paths, p)
	}
	if undigested != "" {
		if budgeted {
			return fmt.Errorf("incomplete layer metadata: file %q in layer %d has no sha256digest "+
				"in its mtree, so duplicated bytes cannot be computed "+
				"(//img/settings:mtree_options must include \"sha256\")", undigested, undigestedLayer+1)
		}
		// Reported as unmeasured rather than as zero: without digests the
		// grouping says nothing about the image.
		return nil
	}

	var groups []duplicateGroup
	var total int64
	for _, group := range byDigest {
		if len(group.Paths) < 2 {
			continue
		}
		sort.Strings(group.Paths)
		total += group.ExtraBytes()
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if a, b := groups[i].ExtraBytes(), groups[j].ExtraBytes(); a != b {
			return a > b
		}
		return groups[i].Paths[0] < groups[j].Paths[0]
	})
	r.rep.DuplicatesMeasured = true
	r.rep.Duplicates = groups
	r.rep.DuplicatedBytes = total
	return nil
}

func fileSize(e mtree.ParsedEntry, layer int) (int64, error) {
	raw, ok := e.Keywords["size"]
	if !ok {
		return 0, fmt.Errorf("incomplete layer metadata: file %q in layer %d has no size "+
			"in its mtree, so wasted bytes cannot be computed "+
			"(//img/settings:mtree_options must include \"size\")", e.Path, layer+1)
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("parsing size %q of file %q in layer %d", raw, e.Path, layer+1)
	}
	return size, nil
}

func parseInt(raw string, fallback int64) int64 {
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return v
}

// parentDir returns the directory part of a canonical mtree path, or "" for
// a top-level entry.
func parentDir(p string) string {
	dir := path.Dir(p)
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}
