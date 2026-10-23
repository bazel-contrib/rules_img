package ext4

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
// owner, timestamps (to the second) and extended attributes. To avoid reading
// files that cannot have a duplicate, content is only hashed for files that
// agree on all of that and on their size.
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

	merged := map[*node]*node{}
	for _, group := range candidates {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].seq < group[j].seq })
		byDigest := map[string][]*node{}
		for _, n := range group {
			if n.digest == nil {
				d, err := digestSource(n)
				if err != nil {
					return err
				}
				n.digest = d
			}
			byDigest[string(n.digest)] = append(byDigest[string(n.digest)], n)
		}
		for _, same := range byDigest {
			keep, links := same[0], refs[same[0]]
			for _, n := range same[1:] {
				if links+refs[n] > maxLinks {
					keep, links = n, refs[n]
					continue
				}
				merged[n] = keep
				links += refs[n]
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	for _, dir := range dirs {
		for name, child := range dir.children {
			if keep, ok := merged[child]; ok {
				dir.children[name] = keep
			}
		}
	}
	return nil
}

// metadataKey identifies the files that may be merged if their content is
// the same. It mirrors the node hash of tar layers: the metadata of the
// file and its size, with timestamps rounded to the second.
func (n *node) metadataKey() string {
	var sb strings.Builder
	for _, v := range []int64{
		int64(n.mode), int64(n.uid), int64(n.gid), n.size,
		n.atime.Unix(), n.ctime.Unix(), n.mtime.Unix(), int64(len(n.xattrs)),
	} {
		sb.WriteString(strconv.FormatInt(v, 10))
		sb.WriteByte(' ')
	}
	for _, x := range n.xattrs {
		fmt.Fprintf(&sb, "%d %q %q ", x.index, x.name, x.value)
	}
	return sb.String()
}

func digestSource(n *node) ([]byte, error) {
	rc, err := n.src.open()
	if err != nil {
		return nil, fmt.Errorf("ext4: %s: %w", n.planned.path, err)
	}
	defer rc.Close()
	h := sha256.New()
	copied, err := io.Copy(h, io.LimitReader(rc, n.size))
	if err == nil && copied != n.size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return nil, fmt.Errorf("ext4: reading content for deduplication: %w", err)
	}
	return h.Sum(nil), nil
}
