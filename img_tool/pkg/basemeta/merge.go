package basemeta

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/baselayer"
)

// MergeOptions controls how streams are combined by [Merge].
type MergeOptions struct {
	// CreateParentDirectories synthesizes a directory entry for every parent of
	// an entry that no stream describes.
	CreateParentDirectories bool
	// ParentDirectoryMode is the mode given to synthesized parents.
	ParentDirectoryMode int64
}

// DefaultMergeOptions returns the options used when a caller has no preference.
func DefaultMergeOptions() MergeOptions {
	return MergeOptions{ParentDirectoryMode: 0o755}
}

// Merge combines the entries of several streams into the ordered entry list of
// a single flat layer.
//
// Streams are supplied in dependency order, and for a path described by more
// than one stream the last one wins -- a rule closer to the layer overrides
// what it depends on. Two entries for the same path with *different* types are
// rejected instead: that is a rule authoring mistake (say a directory shadowing
// a symlink) rather than a deliberate override, and silently picking one would
// produce a subtly broken image.
//
// The result is sorted by path, which puts every parent directory ahead of its
// children.
func Merge(streams [][]*baselayer.BaseEntry, opts MergeOptions) ([]*baselayer.BaseEntry, error) {
	byPath := make(map[string]*baselayer.BaseEntry)
	for _, stream := range streams {
		for _, entry := range stream {
			normalized := NormalizePath(entry.GetPath())
			if normalized == "" {
				return nil, fmt.Errorf("base metadata entry has an empty path (produced by %s)", producerName(entry))
			}
			entry.Path = normalized

			previous, seen := byPath[normalized]
			if seen && previous.GetType() != entry.GetType() {
				return nil, fmt.Errorf(
					"conflicting base metadata for %q: %s describes it as %s, %s as %s",
					normalized,
					producerName(previous), typeName(previous.GetType()),
					producerName(entry), typeName(entry.GetType()),
				)
			}
			byPath[normalized] = entry
		}
	}

	if opts.CreateParentDirectories {
		mode := opts.ParentDirectoryMode
		if mode == 0 {
			mode = 0o755
		}
		for _, entry := range collectPaths(byPath) {
			for _, parent := range parentPaths(entry) {
				if _, exists := byPath[parent]; !exists {
					byPath[parent] = Dir(parent, mode)
				}
			}
		}
	}

	if err := checkAncestorsAreDirectories(byPath); err != nil {
		return nil, err
	}

	merged := make([]*baselayer.BaseEntry, 0, len(byPath))
	for _, p := range collectPaths(byPath) {
		merged = append(merged, byPath[p])
	}
	return merged, nil
}

// checkAncestorsAreDirectories rejects an entry placed underneath something
// that is not a directory.
//
// The usual way to hit this is a skeleton describing /lib as a symlink into
// /usr while another rule places /lib/libc.so.6 directly. Nothing about either
// entry is wrong on its own, so the type-conflict check above does not see it,
// but the resulting image is broken: a file cannot live inside a symlink. It is
// almost always a mismatched usr_merged setting between two rules.
func checkAncestorsAreDirectories(byPath map[string]*baselayer.BaseEntry) error {
	for _, p := range collectPaths(byPath) {
		for _, parent := range parentPaths(p) {
			ancestor, exists := byPath[parent]
			if !exists || ancestor.GetType() == baselayer.EntryType_ENTRY_TYPE_DIRECTORY {
				continue
			}
			return fmt.Errorf(
				"%s places %q underneath %q, which %s describes as a %s rather than a directory",
				producerName(byPath[p]), p, parent,
				producerName(ancestor), typeName(ancestor.GetType()),
			)
		}
	}
	return nil
}

// collectPaths returns the sorted keys of the map. Sorting by path is what
// guarantees parents precede children ("usr" < "usr/lib" because "/" only ever
// extends a prefix).
func collectPaths(byPath map[string]*baselayer.BaseEntry) []string {
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// parentPaths returns every ancestor directory of p, outermost first.
func parentPaths(p string) []string {
	var parents []string
	for dir := path.Dir(p); dir != "." && dir != "/" && dir != ""; dir = path.Dir(dir) {
		parents = append(parents, dir)
	}
	// Reverse so callers see "usr" before "usr/lib".
	for i, j := 0, len(parents)-1; i < j; i, j = i+1, j-1 {
		parents[i], parents[j] = parents[j], parents[i]
	}
	return parents
}

func producerName(entry *baselayer.BaseEntry) string {
	if p := entry.GetProducer(); p != "" {
		return p
	}
	return "<unknown rule>"
}

func typeName(entryType baselayer.EntryType) string {
	name := baselayer.EntryType_name[int32(entryType)]
	if name == "" {
		return fmt.Sprintf("type %d", entryType)
	}
	return strings.ToLower(strings.TrimPrefix(name, "ENTRY_TYPE_"))
}
