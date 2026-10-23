package fat32

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf16"
)

// planned holds the layout decisions for a node.
type planned struct {
	path string
	// parent is the directory containing a directory node (the root is its
	// own parent).
	parent *node
	// dirents are the sorted entries of a directory.
	dirents []dirent

	// clusters is the number of clusters of the contiguous run that holds the
	// node, starting at cluster first. Empty files have none (first is 0).
	clusters, first uint32
	// digest is the SHA-256 of the content of a regular file, if requested.
	digest []byte
}

// dirent is an entry of a directory: a short entry, with the long name
// entries in front of it if long is set.
type dirent struct {
	child *node
	name  string
	short [11]byte
	long  bool
}

func (d dirent) slots() int {
	if !d.long {
		return 1
	}
	return 1 + (len(utf16.Encode([]rune(d.name)))+12)/13
}

// layout is the complete plan of an image.
type layout struct {
	sectorSize  uint64
	clusterSize uint64
	spc         uint64 // sectors per cluster

	reserved     uint64 // sectors before the first FAT
	fatSectors   uint64 // sectors of one FAT
	totalSectors uint64
	clusters     uint32 // data clusters
	usedClusters uint32
	dataStart    uint64 // first sector of cluster 2

	nodes    []*node // in the order they were added; the first is the root
	volumeID uint32
	label    [11]byte
	hasLabel bool
}

func ceilDiv(a, b uint64) uint64 { return (a + b - 1) / b }

func (b *Builder) plan() (*layout, error) {
	l := &layout{sectorSize: uint64(b.opts.SectorSize)}
	b.collect(l)
	if err := b.sizeNodes(l); err != nil {
		return nil, err
	}
	if err := b.geometry(l); err != nil {
		return nil, err
	}
	b.assign(l)
	b.deriveIdentity(l)
	return l, nil
}

// collect finds the nodes of the tree and the path of each.
func (b *Builder) collect(l *layout) {
	nodes := []*node{b.root}
	b.root.planned = planned{parent: b.root}

	var walk func(dir *node)
	walk = func(dir *node) {
		names := make([]string, 0, len(dir.children))
		for name := range dir.children {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := dir.children[name]
			child.planned = planned{path: path.Join(dir.planned.path, name)}
			if child.kind == kindDir {
				child.planned.parent = dir
			}
			nodes = append(nodes, child)
			if child.kind == kindDir {
				walk(child)
			}
		}
	}
	walk(b.root)

	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].seq < nodes[j].seq })
	l.nodes = nodes
}

// sizeNodes determines the directory entries and how many clusters every node
// needs.
func (b *Builder) sizeNodes(l *layout) error {
	if b.opts.Label != "" {
		l.label = shortNameOf(b.opts.Label[:min(len(b.opts.Label), 8)], b.opts.Label[min(len(b.opts.Label), 8):])
		l.hasLabel = true
	} else {
		l.label = shortNameOf("NO NAME", "")
	}

	for _, n := range l.nodes {
		if n.kind != kindDir {
			continue
		}
		ents, err := planDir(n)
		if err != nil {
			return err
		}
		n.planned.dirents = ents
		if slots := dirSlots(n, b.root, l.hasLabel); slots > maxDirEntries {
			return fmt.Errorf("fat32: directory %q has %d entries, more than the %d FAT allows", n.planned.path, slots, maxDirEntries)
		}
	}
	return nil
}

// planDir assigns short names to the children of a directory. Names are unique
// regardless of case, both the long names and the short ones.
func planDir(dir *node) ([]dirent, error) {
	names := make([]string, 0, len(dir.children))
	for name := range dir.children {
		names = append(names, name)
	}
	sort.Strings(names)

	taken := map[string]bool{}
	for _, name := range names {
		taken[foldName(name)] = true
	}
	ents := make([]dirent, 0, len(names))
	for _, name := range names {
		e := dirent{child: dir.children[name], name: name}
		if short, ok := exactShort(name); ok {
			e.short = short
			e.long = name != strings.ToUpper(name)
		} else {
			e.long = true
		}
		ents = append(ents, e)
	}
	for i := range ents {
		if ents[i].long && ents[i].short == ([11]byte{}) {
			short, err := generateShort(ents[i].name, func(s string) bool { return taken[s] })
			if err != nil {
				return nil, err
			}
			ents[i].short = short
			taken[displayShort(short)] = true
		}
	}
	return ents, nil
}

// dirSlots is the number of 32 byte entries of a directory: those of its
// children, "." and ".." (except in the root) and the volume label (in the
// root).
func dirSlots(dir, root *node, hasLabel bool) int {
	slots := 0
	for _, e := range dir.planned.dirents {
		slots += e.slots()
	}
	switch {
	case dir != root:
		slots += 2
	case hasLabel:
		slots++
	}
	return slots
}

// nodeClusters is the number of clusters a node occupies with the given
// cluster size.
func nodeClusters(n, root *node, hasLabel bool, clusterSize uint64) uint64 {
	if n.kind == kindReg {
		return ceilDiv(uint64(n.size), clusterSize)
	}
	return max(1, ceilDiv(uint64(dirSlots(n, root, hasLabel))*32, clusterSize))
}

var autoClusterSizes = []struct {
	size  uint64 // cluster size in bytes
	limit uint64 // largest volume size in bytes this cluster size is used for
}{
	{512, 532480 * 512},
	{4096, 16777216 * 512},
	{8192, 33554432 * 512},
	{16384, 67108864 * 512},
	{32768, 1<<63 - 1},
}

// geometry chooses the cluster size, the number of clusters and the size of
// the FATs.
func (b *Builder) geometry(l *layout) error {
	candidates := autoClusterSizes
	if b.opts.ClusterSize != 0 {
		candidates = candidates[:0:0]
		candidates = append(candidates, struct{ size, limit uint64 }{uint64(b.opts.ClusterSize), 1<<63 - 1})
	}
	for i, c := range candidates {
		if c.size < l.sectorSize {
			continue
		}
		g, err := b.fit(l, c.size)
		if err != nil {
			return err
		}
		if g.totalSectors*l.sectorSize > c.limit && i < len(candidates)-1 {
			continue
		}
		l.clusterSize, l.spc = c.size, c.size/l.sectorSize
		l.reserved, l.fatSectors, l.totalSectors = g.reserved, g.fatSectors, g.totalSectors
		l.clusters, l.usedClusters = g.clusters, g.used
		l.dataStart = g.reserved + numFATs*g.fatSectors
		return nil
	}
	return fmt.Errorf("fat32: no cluster size fits")
}

type geom struct {
	reserved, fatSectors, totalSectors uint64
	clusters, used                     uint32
}

// fit computes the geometry for a cluster size: it re-counts the clusters of
// every node for that size, and returns the smallest volume that holds them
// and has at least the number of clusters FAT32 needs, or the volume of
// Options.Size if that is larger.
func (b *Builder) fit(l *layout, clusterSize uint64) (geom, error) {
	secSize := l.sectorSize
	spc := clusterSize / secSize

	var used uint64
	for _, n := range l.nodes {
		used += nodeClusters(n, b.root, l.hasLabel, clusterSize)
	}
	if used > maxClusters {
		return geom{}, fmt.Errorf("fat32: the content needs %d clusters, more than the %d FAT32 can address", used, maxClusters)
	}

	fatFor := func(clusters uint64) uint64 { return ceilDiv((clusters+2)*4, secSize) }
	// build lays out a volume with the given number of clusters. The reserved
	// area grows so that the data region starts at a multiple of the cluster size.
	build := func(clusters uint64) (reserved, fat, total uint64) {
		fat = fatFor(clusters)
		reserved = reservedSectors
		reserved += (spc - (reserved+numFATs*fat)%spc) % spc
		return reserved, fat, reserved + numFATs*fat + clusters*spc
	}

	// The volume ends right after its last cluster, so that every reader
	// counts the same number of clusters. With Options.Size, it has the fewest
	// clusters that make the volume at least that large.
	clusters := max(used, minClusters)
	reserved, fat, total := build(clusters)
	if target := ceilDiv(uint64(b.opts.Size), secSize); total < target {
		// Search for the fewest clusters that reach the target. The volume
		// grows by a cluster and a little of the FATs with every cluster.
		lo, hi := clusters, clusters+ceilDiv(target-total, spc)
		for _, _, t := build(hi); t < target; _, _, t = build(hi) {
			hi *= 2
		}
		for lo < hi {
			mid := lo + (hi-lo)/2
			if _, _, t := build(mid); t >= target {
				hi = mid
			} else {
				lo = mid + 1
			}
		}
		clusters = lo
		reserved, fat, total = build(clusters)
	}
	if clusters > maxClusters {
		return geom{}, fmt.Errorf("fat32: %d clusters are more than the %d FAT32 can address", clusters, maxClusters)
	}
	if total > 1<<32-1 {
		return geom{}, fmt.Errorf("fat32: the volume of %d sectors is larger than FAT32 allows", total)
	}
	return geom{reserved: reserved, fatSectors: fat, totalSectors: total, clusters: uint32(clusters), used: uint32(used)}, nil
}

// assign places every node: directories first, root at cluster 2, then the
// files, each in a contiguous run.
func (b *Builder) assign(l *layout) {
	next := uint32(rootCluster)
	for _, k := range []kind{kindDir, kindReg} {
		for _, n := range l.nodes {
			if n.kind != k {
				continue
			}
			p := &n.planned
			p.clusters = uint32(nodeClusters(n, b.root, l.hasLabel, l.clusterSize))
			if p.clusters > 0 {
				p.first = next
				next += p.clusters
			}
		}
	}
}

// deriveIdentity fills in the volume ID: the one of the options or one derived
// from a digest of the metadata of the tree, so equal trees give equal images.
func (b *Builder) deriveIdentity(l *layout) {
	if b.opts.VolumeID != 0 {
		l.volumeID = b.opts.VolumeID
		return
	}
	h := sha256.New()
	var buf [8]byte
	put := func(v uint64) {
		binary.LittleEndian.PutUint64(buf[:], v)
		h.Write(buf[:])
	}
	for _, n := range l.nodes {
		put(uint64(n.kind))
		put(uint64(n.size))
		put(uint64(n.mtime.Unix()))
		h.Write([]byte(n.planned.path))
		h.Write([]byte{0})
	}
	l.volumeID = binary.LittleEndian.Uint32(h.Sum(nil))
	if l.volumeID == 0 {
		l.volumeID = 1
	}
}
