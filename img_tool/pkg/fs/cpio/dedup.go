package cpio

import (
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// deduplicate merges regular files with identical content and metadata, as
// requested by [Options.Deduplicate]. All entries of the tree that referenced a
// merged file end up referencing the one that is kept, which makes them hard
// links.
//
// Like the tar layers, equality of the metadata is judged on the node: mode,
// owner and timestamp. To avoid reading files that cannot have a duplicate,
// content is only hashed for files that agree on all of that and on their size.
func (b *Builder) deduplicate() error {
	if !b.opts.Deduplicate {
		return nil
	}

	// Find the reachable regular files, and how often they are referenced.
	refs := map[*node]int{}
	var dirs []*node
	seenDirs := map[*node]bool{b.root: true}
	var walk func(dir *node)
	walk = func(dir *node) {
		dirs = append(dirs, dir)
		for _, child := range dir.children {
			refs[child]++
			if child.kind == kindDir && !seenDirs[child] {
				seenDirs[child] = true
				walk(child)
			}
		}
	}
	walk(b.root)

	candidates := map[string][]*node{}
	for n := range refs {
		if n.kind == kindReg && n.dedup {
			key := n.metadataKey()
			candidates[key] = append(candidates[key], n)
		}
	}

	// Hash the files that may have a duplicate and no digest yet. Sources that
	// are streamed (tar entries) are read in the order they were added, which
	// is the order they are cheapest to read in.
	var unhashed []*node
	for _, group := range candidates {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].seq < group[j].seq })
		for _, n := range group {
			if n.digest == nil {
				unhashed = append(unhashed, n)
			}
		}
	}
	sort.Slice(unhashed, func(i, j int) bool { return unhashed[i].seq < unhashed[j].seq })
	for _, n := range unhashed {
		d, err := b.digestOf(n)
		if err != nil {
			return err
		}
		n.digest = d
	}

	merged := map[*node]*node{}
	for _, group := range candidates {
		if len(group) < 2 {
			continue
		}
		byDigest := map[string][]*node{}
		for _, n := range group {
			byDigest[string(n.digest)] = append(byDigest[string(n.digest)], n)
		}
		for _, same := range byDigest {
			keep := same[0]
			for _, n := range same[1:] {
				merged[n] = keep
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	for _, dir := range dirs {
		for name, child := range dir.children {
			if keep, ok := merged[child]; ok {
				// Dropping the last reference to the merged file frees its content.
				b.setChild(dir, name, keep)
			}
		}
	}
	return nil
}

// metadataKey identifies the files that may be merged if their content is
// the same. It is the metadata of the file and its size.
func (n *node) metadataKey() string {
	var sb strings.Builder
	for _, v := range []int64{int64(n.mode), int64(n.uid), int64(n.gid), n.size, n.mtime} {
		sb.WriteString(strconv.FormatInt(v, 10))
		sb.WriteByte(' ')
	}
	return sb.String()
}

// digestOf returns the SHA-256 digest of the content of a regular file. The
// digest of a file read from a path is remembered, so it is computed once no
// matter how often the file is added or needs its digest.
func (b *Builder) digestOf(n *node) ([]byte, error) {
	if n.digest != nil {
		return n.digest, nil
	}
	key, memoizable := pathKeyOf(n)
	if memoizable {
		if d, ok := b.pathDigests[key]; ok {
			return d, nil
		}
	}
	rc, err := n.src.open()
	if err != nil {
		return nil, fmt.Errorf("cpio: reading content for digest: %w", err)
	}
	defer rc.Close()
	h := sha256.New()
	copied, err := io.Copy(h, io.LimitReader(rc, n.size))
	if err == nil && copied != n.size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, fmt.Errorf("cpio: reading content for digest: %w", err)
	}
	d := h.Sum(nil)
	if memoizable {
		b.pathDigests[key] = d
	}
	return d, nil
}

func pathKeyOf(n *node) (pathKey, bool) {
	p, ok := n.src.(pathSource)
	return pathKey{path: string(p), size: n.size}, ok
}

// knownPathDigest returns the remembered digest of a file read from a path.
func (b *Builder) knownPathDigest(n *node) ([]byte, bool) {
	key, ok := pathKeyOf(n)
	if !ok {
		return nil, false
	}
	d, ok := b.pathDigests[key]
	return d, ok
}

func (b *Builder) rememberPathDigest(n *node) {
	if key, ok := pathKeyOf(n); ok {
		b.pathDigests[key] = n.digest
	}
}
