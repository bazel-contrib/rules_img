package iso9660

import (
	"fmt"
	"sort"
	"time"
)

// planned holds the layout decisions for a node. Nodes that several directory
// entries reference (hard links, deduplicated files) are planned once.
type planned struct {
	// path is the first path the node was added at, used for [Info.Files] and
	// for error messages.
	path string
	// nlink is how many directory entries reference the node, which is what
	// the Rock Ridge PX entry and the UDF File Entry record.
	nlink int

	// lba and blocks locate the data of a regular file.
	lba, blocks uint32
	// digest is the SHA-256 of the content of a regular file, if requested.
	digest []byte
	// patched marks a boot image whose boot information table was written into
	// its data, so its stored content differs from the file that went in.
	patched bool

	// udfFE is the block of the node's UDF File Entry.
	udfFE uint32
	// udfData and udfDataLength locate a UDF directory's file identifier
	// descriptors, or the path component list of a symbolic link.
	udfData       uint32
	udfDataLength uint32
	// udfUniqueID is the identifier UDF gives the node.
	udfUniqueID uint64
}

// ceRef locates a Rock Ridge continuation area: the block it starts in, the
// offset within that block and its length.
type ceRef struct{ block, offset, length uint32 }

// record is a planned directory record of one hierarchy.
type record struct {
	// id is the identifier as it is stored: a single zero byte for ".", a
	// single one byte for "..", d-characters in the primary hierarchy and
	// big-endian UCS-2 in the Joliet one.
	id    []byte
	isDir bool
	// node is the entry the record describes.
	node *node
	// dir is set when the record points at a directory of this hierarchy,
	// which is where its extent comes from.
	dir *dirInfo
	// extent is which extent of the entry the record describes. A file larger
	// than maxExtentLength needs one record per extent, all with the same
	// identifier and all but the last marked as continuing.
	extent int
	// lastExtent reports whether this is the final record of the entry.
	lastExtent bool

	// susp is the system use area stored in the record, overflow what went
	// into the continuation area at ce.
	susp     []suspEntry
	overflow []suspEntry
	ce       ceRef
	// size is the total length of the record, always even.
	size int
}

// location is the block and length of what the record points at. For a file
// recorded in several extents it is the record's own extent, which follows the
// previous ones: the writer stores every file as one contiguous run.
func (r *record) location() (lba, length uint32) {
	if r.dir != nil {
		return r.dir.lba, r.dir.length
	}
	if r.node.kind != kindReg {
		return 0, 0
	}
	offset := int64(r.extent) * maxExtentLength
	return r.node.planned.lba + uint32(offset/BlockSize), uint32(min(r.node.size-offset, maxExtentLength))
}

// extentsFor is how many directory records an entry needs: one per extent, and
// one for anything that is not a file with content.
func extentsFor(n *node) int {
	if n.kind != kindReg || n.size <= maxExtentLength {
		return 1
	}
	return int((n.size + maxExtentLength - 1) / maxExtentLength)
}

// dirInfo is a directory of one hierarchy.
type dirInfo struct {
	node   *node
	path   string
	parent *dirInfo
	// number is the one-based record number of the directory in the path
	// table. The root is 1.
	number uint16
	// records are the records of the directory extent, in the order ISO 9660
	// requires: ".", "..", then the children by identifier.
	records []*record
	// lba and length locate the directory extent.
	lba, length uint32
}

// hierarchy is one directory tree of the image: the primary one or the Joliet
// one. Both describe the same files, with their own names, directory extents
// and path tables.
type hierarchy struct {
	joliet bool
	dirs   []*dirInfo
	// pathTableSize is the length of either path table in bytes.
	pathTableSize uint32
	// lPathTable and mPathTable are the blocks of the little-endian and the
	// big-endian path table.
	lPathTable, mPathTable uint32
}

func (h *hierarchy) root() *dirInfo { return h.dirs[0] }

// layout is the complete plan of an image.
type layout struct {
	totalBlocks uint32

	// Volume descriptors, in the order they appear from block 16 on.
	pvdLBA, brvdLBA, svdLBA, vdstLBA uint32
	beaLBA, nsrLBA, teaLBA           uint32

	bootCatalogLBA uint32
	boot           []bootEntryPlan

	primary *hierarchy
	joliet  *hierarchy

	// continuation holds the Rock Ridge continuation areas.
	ceStart, ceBlocks uint32

	udf *udfLayout

	// nodes are the unique nodes of the tree, in the order they were added;
	// the first is the root. files are the regular files with content, in the
	// order their data was laid out.
	nodes []*node
	files []*node

	time time.Time
}

// bootEntryPlan is an El Torito entry with its boot image resolved.
type bootEntryPlan struct {
	BootEntry
	node *node
}

func (b *Builder) plan() (*layout, error) {
	if err := b.deduplicate(); err != nil {
		return nil, err
	}
	l := &layout{time: b.opts.Time}
	b.collectNodes(l)

	var err error
	if l.primary, err = b.buildHierarchy(l, false); err != nil {
		return nil, err
	}
	if b.opts.Joliet {
		if l.joliet, err = b.buildHierarchy(l, true); err != nil {
			return nil, err
		}
	}
	if err := b.resolveBoot(l); err != nil {
		return nil, err
	}
	if b.opts.UDF {
		if err := b.planUDF(l); err != nil {
			return nil, err
		}
	}
	return l, b.allocate(l)
}

// collectNodes finds the unique nodes of the tree, the first path of each and
// how often each is referenced.
func (b *Builder) collectNodes(l *layout) {
	seen := map[*node]bool{b.root: true}
	b.root.planned = planned{nlink: 1}
	l.nodes = []*node{b.root}

	var walk func(dir *node, dirPath string)
	walk = func(dir *node, dirPath string) {
		names := sortedNames(dir)
		for _, name := range names {
			child := dir.children[name]
			p := name
			if dirPath != "" {
				p = dirPath + "/" + name
			}
			if !seen[child] {
				seen[child] = true
				child.planned = planned{path: p}
				l.nodes = append(l.nodes, child)
			}
			child.planned.nlink++
			if child.kind == kindDir {
				walk(child, p)
			}
		}
	}
	walk(b.root, "")

	sort.SliceStable(l.nodes, func(i, j int) bool { return l.nodes[i].seq < l.nodes[j].seq })
	for _, n := range l.nodes {
		// An entry with no timestamp of its own, such as a directory nothing
		// described, takes the default time of the image. Resolving it here
		// keeps the directory records, the Rock Ridge timestamps and the UDF
		// File Entries from each inventing their own fallback.
		for _, t := range []*time.Time{&n.mtime, &n.atime, &n.ctime} {
			if t.IsZero() {
				*t = b.opts.Time
			}
		}
		// A directory's link count is the number of its subdirectories plus
		// the "." entry it holds and the ".." entry of each child, as POSIX
		// reports it: two plus the number of subdirectories.
		if n.kind != kindDir {
			continue
		}
		links := 2
		for _, child := range n.children {
			if child.kind == kindDir {
				links++
			}
		}
		n.planned.nlink = links
	}
}

func sortedNames(dir *node) []string {
	names := make([]string, 0, len(dir.children))
	for name := range dir.children {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// buildHierarchy lays out one directory tree: it assigns the identifiers,
// orders the directories the way the path table requires (by level, then by
// parent, then by identifier) and plans every directory record.
func (b *Builder) buildHierarchy(l *layout, joliet bool) (*hierarchy, error) {
	h := &hierarchy{joliet: joliet}
	root := &dirInfo{node: b.root}
	root.parent = root
	h.dirs = []*dirInfo{root}

	jolietLimit := maxJolietChars
	if b.opts.JolietLongNames {
		jolietLimit = maxJolietLongChars
	}

	// Breadth-first over a growing slice: children are appended behind every
	// directory of the current level, so the result is ordered by level, and
	// within a level by parent and then by identifier.
	for i := 0; i < len(h.dirs); i++ {
		if i+1 > 0xFFFF {
			return nil, fmt.Errorf("iso9660: more than %d directories, which the path table cannot number", 0xFFFF)
		}
		dir := h.dirs[i]
		dir.number = uint16(i + 1)

		type entry struct {
			name  string
			id    []byte
			child *node
		}
		taken := map[string]bool{}
		var entries []entry
		for _, name := range sortedNames(dir.node) {
			child := dir.node.children[name]
			var id string
			var err error
			if joliet {
				id, err = jolietIdentifier(name, jolietLimit, func(s string) bool { return taken[s] })
			} else {
				id, err = isoIdentifier(name, child.kind == kindDir, func(s string) bool { return taken[s] })
			}
			if err != nil {
				return nil, fmt.Errorf("iso9660: %s: %w", pathOf(dir.path, name), err)
			}
			taken[id] = true
			stored := []byte(id)
			if joliet {
				stored = ucs2(id)
			}
			entries = append(entries, entry{name: name, id: stored, child: child})
		}
		unit := identifierUnit(joliet)
		sort.SliceStable(entries, func(x, y int) bool {
			return compareIdentifiers(entries[x].id, entries[y].id, unit) < 0
		})

		dir.records = []*record{
			{id: []byte{0}, isDir: true, node: dir.node, dir: dir, lastExtent: true},
			{id: []byte{1}, isDir: true, node: dir.parent.node, dir: dir.parent, lastExtent: true},
		}
		for _, e := range entries {
			// The records of one entry are consecutive and share an
			// identifier, so the sort above decides their place as a group.
			extents := extentsFor(e.child)
			for i := range extents {
				rec := &record{
					id:         e.id,
					isDir:      e.child.kind == kindDir,
					node:       e.child,
					extent:     i,
					lastExtent: i == extents-1,
				}
				if e.child.kind == kindDir {
					child := &dirInfo{node: e.child, path: pathOf(dir.path, e.name), parent: dir}
					rec.dir = child
					h.dirs = append(h.dirs, child)
				}
				dir.records = append(dir.records, rec)
				if !joliet && b.opts.RockRidge {
					// Every record of a multi-extent file carries the same
					// system use entries, so a reader sees one file however
					// many records it reads.
					if err := b.planRockRidge(rec, e.name); err != nil {
						return nil, fmt.Errorf("iso9660: %s: %w", pathOf(dir.path, e.name), err)
					}
				}
			}
		}
		if !joliet && b.opts.RockRidge {
			b.planDotRockRidge(dir, i == 0)
		}
		for _, rec := range dir.records {
			if err := sizeRecord(rec); err != nil {
				return nil, fmt.Errorf("iso9660: %s: %w", dir.path, err)
			}
		}
	}

	// The extent of a directory holds its records, none of which crosses a
	// block boundary.
	for _, dir := range h.dirs {
		dir.length = uint32(directoryExtentSize(dir.records))
	}
	h.pathTableSize = pathTableSize(h.dirs)
	return h, nil
}

func pathOf(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// planRockRidge builds the system use entries of a record describing a child
// entry.
func (b *Builder) planRockRidge(rec *record, name string) error {
	n := rec.node
	entries := []suspEntry{
		rripPX(isoMode(n), n.planned.nlink, n.uid, n.gid),
		rripTF(n.mtime, n.atime, n.ctime),
	}
	entries = append(entries, rripNM(name)...)
	switch n.kind {
	case kindSymlink:
		sl, err := rripSL(n.target)
		if err != nil {
			return err
		}
		entries = append(entries, sl...)
	case kindChar, kindBlock:
		entries = append(entries, rripPN(n.major, n.minor))
	}
	rec.susp = entries
	return nil
}

// planDotRockRidge builds the system use entries of the "." and ".." records
// of a directory. The "." record of the root also carries the SUSP indicator
// and, in its continuation area, the Rock Ridge extension reference.
func (b *Builder) planDotRockRidge(dir *dirInfo, isRoot bool) {
	dot, dotdot := dir.records[0], dir.records[1]
	if isRoot {
		dot.susp = []suspEntry{suspSP()}
	}
	dot.susp = append(dot.susp,
		rripPX(isoMode(dir.node), dir.node.planned.nlink, dir.node.uid, dir.node.gid),
		rripTF(dir.node.mtime, dir.node.atime, dir.node.ctime),
	)
	if isRoot {
		dot.susp = append(dot.susp, suspER())
	}
	parent := dir.parent.node
	dotdot.susp = []suspEntry{
		rripPX(isoMode(parent), parent.planned.nlink, parent.uid, parent.gid),
		rripTF(parent.mtime, parent.atime, parent.ctime),
	}
}

// isoMode is the POSIX mode of a node, the permission bits together with the
// file type bits.
func isoMode(n *node) uint32 {
	mode := n.mode & 0o7777
	switch n.kind {
	case kindDir:
		mode |= 0o040000
	case kindReg:
		mode |= 0o100000
	case kindSymlink:
		mode |= 0o120000
	case kindChar:
		mode |= 0o020000
	case kindBlock:
		mode |= 0o060000
	case kindFifo:
		mode |= 0o010000
	}
	return mode
}

// sizeRecord splits the system use area of a record between the record itself
// and a continuation area, and computes the total length of the record.
func sizeRecord(rec *record) error {
	base := 33 + len(rec.id)
	if base%2 != 0 {
		base++ // the padding field that keeps records on even boundaries
	}
	if base > maxRecordLength {
		return fmt.Errorf("the identifier of %d bytes does not fit in a directory record", len(rec.id))
	}
	// One byte is held back so that an odd system use area can be padded to an
	// even record length.
	inline, overflow := splitSUSP(rec.susp, maxRecordLength-base-1)
	used := suspSize(inline)
	if len(overflow) > 0 {
		used += ceEntryLength
		length := suspSize(overflow)
		if length > BlockSize {
			return fmt.Errorf("the Rock Ridge metadata of %d bytes does not fit in a continuation area", length)
		}
		rec.ce.length = uint32(length)
	}
	rec.susp, rec.overflow = inline, overflow
	size := base + used
	if size%2 != 0 {
		size++
	}
	rec.size = size
	return nil
}

// directoryExtentSize is how many bytes the records of a directory need. A
// record never crosses a logical block boundary, so a block that cannot hold
// the next record is left padded with zeros.
func directoryExtentSize(records []*record) int64 {
	var total, inBlock int64
	for _, rec := range records {
		if inBlock+int64(rec.size) > BlockSize {
			total += BlockSize - inBlock
			inBlock = 0
		}
		inBlock += int64(rec.size)
		total += int64(rec.size)
	}
	return (total + BlockSize - 1) / BlockSize * BlockSize
}

// pathTableSize is the length of a path table describing the directories.
func pathTableSize(dirs []*dirInfo) uint32 {
	var total uint32
	for _, dir := range dirs {
		total += pathTableRecordSize(dir)
	}
	return total
}

func pathTableRecordSize(dir *dirInfo) uint32 {
	size := uint32(8 + len(pathTableIdentifier(dir)))
	if size%2 != 0 {
		size++
	}
	return size
}

// pathTableIdentifier is the identifier a path table record carries: the one
// the directory has in its parent, or a single zero byte for the root.
func pathTableIdentifier(dir *dirInfo) []byte {
	if dir.parent == dir {
		return []byte{0}
	}
	for _, rec := range dir.parent.records {
		if rec.dir == dir {
			return rec.id
		}
	}
	return []byte{0}
}

// resolveBoot finds the boot images of the El Torito entries in the finished
// tree.
func (b *Builder) resolveBoot(l *layout) error {
	for i, e := range b.opts.Boot {
		parts, err := splitPath(e.Image)
		if err != nil {
			return err
		}
		n := b.root
		for _, c := range parts {
			if n.kind != kindDir || n.children[c] == nil {
				n = nil
				break
			}
			n = n.children[c]
		}
		if n == nil {
			return fmt.Errorf("iso9660: boot entry %d: %q is not in the image", i, e.Image)
		}
		if n.kind != kindReg || n.size == 0 {
			return fmt.Errorf("iso9660: boot entry %d: %q is not a regular file with content", i, e.Image)
		}
		if e.BootInfoTable {
			if n.planned.nlink > 1 {
				return fmt.Errorf("iso9660: boot entry %d: %q shares its content with another entry, so its boot information table cannot be patched in", i, e.Image)
			}
			if n.size < bootInfoTableEnd {
				return fmt.Errorf("iso9660: boot entry %d: %q is only %d bytes, too small for a boot information table", i, e.Image, n.size)
			}
			n.planned.patched = true
		}
		l.boot = append(l.boot, bootEntryPlan{BootEntry: e, node: n})
	}
	return nil
}

// allocate assigns a block to every structure of the image. The data of the
// regular files comes last, in one run, so that the image can be written (and
// hashed) front to back once the metadata is in place.
func (b *Builder) allocate(l *layout) error {
	next := uint32(systemAreaBlocks)
	take := func(blocks uint32) uint32 {
		lba := next
		next += blocks
		return lba
	}

	l.pvdLBA = take(1)
	if len(l.boot) > 0 {
		l.brvdLBA = take(1)
	}
	if b.opts.Joliet {
		l.svdLBA = take(1)
	}
	l.vdstLBA = take(1)
	if b.opts.UDF {
		// The UDF volume recognition sequence continues the one ISO 9660
		// started, so it directly follows the descriptor set terminator.
		l.beaLBA = take(1)
		l.nsrLBA = take(1)
		l.teaLBA = take(1)
		// Everything else moves past the blocks whose use UDF fixes: the two
		// volume descriptor sequences, the integrity sequence and the first
		// anchor.
		next = udfContentStart
	}

	if len(l.boot) > 0 {
		l.bootCatalogLBA = take(1)
	}
	for _, h := range l.hierarchies() {
		h.lPathTable = take(blocksFor(int64(h.pathTableSize)))
		h.mPathTable = take(blocksFor(int64(h.pathTableSize)))
	}
	for _, h := range l.hierarchies() {
		for _, dir := range h.dirs {
			dir.lba = take(dir.length / BlockSize)
		}
	}

	// The Rock Ridge continuation areas are packed into blocks, with no area
	// crossing a block boundary.
	if ceBlocks, err := l.assignContinuations(next); err != nil {
		return err
	} else if ceBlocks > 0 {
		l.ceStart = next
		l.ceBlocks = ceBlocks
		next += ceBlocks
	}

	if b.opts.UDF {
		next = l.udf.allocate(l, next)
	}

	for _, n := range l.nodes {
		if n.kind != kindReg || n.size == 0 {
			continue
		}
		n.planned.blocks = blocksFor(n.size)
		n.planned.lba = take(n.planned.blocks)
		l.files = append(l.files, n)
	}

	// The total is counted in 64 bits: the block counts of the format are 32
	// bits wide, so a volume that does not fit has to be rejected rather than
	// silently truncated into one that does.
	total := uint64(next)
	if b.opts.UDF {
		total++ // the anchor in the last block of the volume
	}
	total = max(total, uint64((b.opts.Size+BlockSize-1)/BlockSize))
	if total > maxVolumeBlocks {
		return fmt.Errorf("iso9660: the image of %d blocks is larger than the %d blocks the format can describe", total, uint64(maxVolumeBlocks))
	}
	l.totalBlocks = uint32(total)
	if b.opts.UDF {
		l.udf.partitionBlocks = l.totalBlocks - 1 - l.udf.partitionStart
	}
	return nil
}

func (l *layout) hierarchies() []*hierarchy {
	if l.joliet == nil {
		return []*hierarchy{l.primary}
	}
	return []*hierarchy{l.primary, l.joliet}
}

// assignContinuations places every record's Rock Ridge overflow in the
// continuation area and returns how many blocks it needs.
func (l *layout) assignContinuations(start uint32) (uint32, error) {
	var block, offset uint32
	used := false
	for _, dir := range l.primary.dirs {
		for _, rec := range dir.records {
			if len(rec.overflow) == 0 {
				continue
			}
			if offset+rec.ce.length > BlockSize {
				block++
				offset = 0
			}
			rec.ce.block = start + block
			rec.ce.offset = offset
			offset += rec.ce.length
			used = true
		}
	}
	if !used {
		return 0, nil
	}
	return block + 1, nil
}
