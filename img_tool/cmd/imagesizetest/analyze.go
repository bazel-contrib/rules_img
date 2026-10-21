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

// wasteItem is one shipped file copy that no process can see in the final
// filesystem because a later layer shadowed it.
type wasteItem struct {
	Path          string
	Size          int64
	WrittenLayer  int // 0-based layer index that shipped the bytes
	ShadowedLayer int // 0-based layer index that made them unreachable
	Reason        string
}

// duplicateGroup is identical file content live at more than one path.
type duplicateGroup struct {
	Digest string
	Size   int64 // size of one copy
	Paths  []string
}

// ExtraBytes is the cost of the copies beyond the first.
func (g duplicateGroup) ExtraBytes() int64 {
	return g.Size * int64(len(g.Paths)-1)
}

type report struct {
	WastedBytes     int64
	Waste           []wasteItem // largest first
	DuplicatedBytes int64
	Duplicates      []duplicateGroup // largest extra cost first
}

// liveEntry is a regular file currently visible at a path while replaying
// layers. size is the shipped bytes still carried by this path: for a
// hardlinked file (nlink > 1) the content ships once per layer, so only one
// sibling path carries the bytes at a time.
type liveEntry struct {
	layer    int
	size     int64
	fullSize int64
	digest   string
	nlink    int64
}

// analyze replays the per-layer mtrees in layer order and computes the wasted
// bytes (shipped but shadowed by an overwrite, whiteout, or opaque whiteout
// in a later layer) and, when checkDuplicates is set, the duplicated bytes
// (identical sha256digest content live at multiple paths, hardlinks and
// symlinks excluded). Only regular files carry bytes; sizes are the
// uncompressed tar entry sizes recorded in the mtree.
func analyze(layers []layerInput, checkDuplicates bool) (*report, error) {
	for i, layer := range layers {
		if layer.missing {
			return nil, fmt.Errorf("incomplete layer metadata: layer %d of %d has no mtree "+
				"(a shallow base-image layer whose blob is never fetched?), so wasted bytes "+
				"cannot be computed for the whole image", i+1, len(layers))
		}
	}

	rep := &report{}
	live := map[string]*liveEntry{}

	shadow := func(p string, rec *liveEntry, byLayer int, reason string) {
		delete(live, p)
		if rec.size == 0 {
			return
		}
		// A hardlink group's content ships once. If the path carrying the
		// bytes is shadowed but a sibling path still exposes the same
		// shipped copy, hand the bytes to the sibling instead of counting
		// them as wasted.
		if rec.nlink > 1 && rec.digest != "" {
			for _, sibPath := range sortedKeys(live) {
				sib := live[sibPath]
				if sib.layer == rec.layer && sib.digest == rec.digest && sib.nlink > 1 && sib.size == 0 {
					sib.size = sib.fullSize
					return
				}
			}
		}
		rep.WastedBytes += rec.size
		rep.Waste = append(rep.Waste, wasteItem{
			Path:          p,
			Size:          rec.size,
			WrittenLayer:  rec.layer,
			ShadowedLayer: byLayer,
			Reason:        reason,
		})
	}

	// shadowWhere shadows every live entry whose path matches pred, in path
	// order so hardlink byte hand-off is deterministic.
	shadowWhere := func(byLayer int, reason string, pred func(string, *liveEntry) bool) {
		for _, p := range sortedKeys(live) {
			if rec := live[p]; pred(p, rec) {
				shadow(p, rec, byLayer, reason)
			}
		}
	}

	for li, layer := range layers {
		// Digests whose shipped bytes a hardlink entry already carries in
		// this layer; further nlink>1 entries with the same digest in the
		// same layer are extra names for the same shipped copy.
		hardlinkCarrier := map[string]bool{}
		for _, e := range layer.entries {
			p := e.Path
			if p == "." || p == "" {
				continue
			}
			base := path.Base(p)
			parent := parentDir(p)
			switch {
			case base == ".wh..wh..opq":
				// Opaque whiteout: hide everything lower layers put in
				// this directory. Entries from this layer survive.
				shadowWhere(li, "hidden by opaque whiteout", func(q string, rec *liveEntry) bool {
					return rec.layer < li && isUnder(q, parent)
				})
				continue
			case strings.HasPrefix(base, ".wh."):
				target := strings.TrimPrefix(base, ".wh.")
				if parent != "" {
					target = parent + "/" + target
				}
				shadowWhere(li, "deleted", func(q string, _ *liveEntry) bool {
					return q == target || isUnder(q, target)
				})
				continue
			}

			typ := e.Keywords["type"]
			// A file (or other non-directory) replaces whatever a lower
			// layer had at this path and below it, and anything below a
			// lower layer's file replaces that file with a directory. A
			// directory only shadows a file at its own path; directories
			// merge.
			shadowWhere(li, "overwritten", func(q string, _ *liveEntry) bool {
				if q == p {
					return true
				}
				if typ != "dir" && isUnder(q, p) {
					return true
				}
				return isUnder(p, q)
			})

			if typ != "file" {
				continue
			}
			size, err := fileSize(e, li)
			if err != nil {
				return nil, err
			}
			nlink := parseInt(e.Keywords["nlink"], 1)
			digest := e.Keywords["sha256digest"]
			effective := size
			if nlink > 1 && digest != "" {
				if hardlinkCarrier[digest] {
					effective = 0
				} else {
					hardlinkCarrier[digest] = true
				}
			}
			live[p] = &liveEntry{layer: li, size: effective, fullSize: size, digest: digest, nlink: nlink}
		}
	}

	if checkDuplicates {
		dups, total, err := duplicates(live)
		if err != nil {
			return nil, err
		}
		rep.Duplicates = dups
		rep.DuplicatedBytes = total
	}

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

// duplicates groups live regular files by content digest. Hardlinks
// (nlink > 1) ship one copy for many paths and empty files cost nothing, so
// neither counts.
func duplicates(live map[string]*liveEntry) ([]duplicateGroup, int64, error) {
	byDigest := map[string][]string{}
	sizes := map[string]int64{}
	for _, p := range sortedKeys(live) {
		rec := live[p]
		if rec.fullSize == 0 || rec.nlink > 1 {
			continue
		}
		if rec.digest == "" {
			return nil, 0, fmt.Errorf("incomplete layer metadata: file %q in layer %d has no sha256digest "+
				"in its mtree, so duplicated bytes cannot be computed "+
				"(//img/settings:mtree_options must include \"sha256\")", p, rec.layer+1)
		}
		byDigest[rec.digest] = append(byDigest[rec.digest], p)
		sizes[rec.digest] = rec.fullSize
	}
	var groups []duplicateGroup
	var total int64
	for digest, paths := range byDigest {
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths)
		g := duplicateGroup{Digest: digest, Size: sizes[digest], Paths: paths}
		total += g.ExtraBytes()
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if a, b := groups[i].ExtraBytes(), groups[j].ExtraBytes(); a != b {
			return a > b
		}
		return groups[i].Paths[0] < groups[j].Paths[0]
	})
	return groups, total, nil
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

func sortedKeys(live map[string]*liveEntry) []string {
	keys := make([]string, 0, len(live))
	for k := range live {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parentDir returns the directory part of a canonical mtree path, or "" for
// a top-level entry.
func parentDir(p string) string {
	dir := path.Dir(p)
	if dir == "." {
		return ""
	}
	return dir
}

// isUnder reports whether q is strictly below directory dir. The root ("")
// contains every path.
func isUnder(q, dir string) bool {
	if dir == "" {
		return true
	}
	return strings.HasPrefix(q, dir+"/")
}

// humanBytes renders a byte count like the issue's example output (91.4 MB).
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
