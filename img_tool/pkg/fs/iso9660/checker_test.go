package iso9660

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"
)

// This file contains an independent reader and checker for the images the
// package writes. It only relies on the on-disk format (ECMA-119 for ISO 9660,
// IEEE P1281 and P1282 for Rock Ridge, the El Torito specification and
// ECMA-167 for UDF), not on the writer's bookkeeping, so it catches layout
// mistakes the way a filesystem checker would.

type checker struct {
	t      *testing.T
	r      io.ReaderAt
	size   int64
	blocks uint32

	// used maps every block the image claims to what claims it, so that two
	// structures cannot overlap.
	used map[uint32]string

	rockRidge bool
	// entries are the paths the primary hierarchy holds.
	entries map[string]*checkedEntry
}

type checkedEntry struct {
	path        string
	isDir       bool
	lba, length uint32
	susp        []suspValue
}

func check(t *testing.T, img *memImage) *checker {
	t.Helper()
	c := &checker{
		t:       t,
		r:       img,
		size:    int64(len(img.data)),
		used:    map[uint32]string{},
		entries: map[string]*checkedEntry{},
	}
	c.run()
	return c
}

func (c *checker) block(lba uint32) []byte {
	c.t.Helper()
	b := make([]byte, BlockSize)
	if err := readFull(c.r, b, int64(lba)*BlockSize); err != nil {
		c.t.Fatalf("reading block %d: %v", lba, err)
	}
	return b
}

// claim records that a run of blocks belongs to what. A block two structures
// claim is a layout bug.
func (c *checker) claim(lba, blocks uint32, what string) {
	c.t.Helper()
	for i := uint32(0); i < blocks; i++ {
		if lba+i >= c.blocks && c.blocks != 0 {
			c.t.Errorf("%s claims block %d, which is past the end of the %d block volume", what, lba+i, c.blocks)
			return
		}
		if other, ok := c.used[lba+i]; ok {
			c.t.Errorf("block %d is claimed by both %s and %s", lba+i, other, what)
			return
		}
		c.used[lba+i] = what
	}
}

func (c *checker) run() {
	c.t.Helper()
	if c.size%BlockSize != 0 {
		c.t.Errorf("the image is %d bytes, which is not a whole number of %d byte blocks", c.size, BlockSize)
	}
	for i := int64(0); i < systemAreaBlocks*BlockSize; i++ {
		if c.size > i && readByte(c.t, c.r, i) != 0 {
			c.t.Errorf("the system area is not all zero: byte %d is set", i)
			break
		}
	}

	pvd := c.block(systemAreaBlocks)
	c.checkVolumeDescriptor(pvd, 1, false)
	c.blocks = binary.LittleEndian.Uint32(pvd[80:])
	if int64(c.blocks)*BlockSize != c.size {
		c.t.Errorf("the volume declares %d blocks but the image is %d bytes", c.blocks, c.size)
	}
	c.claim(0, systemAreaBlocks, "system area")
	c.claim(systemAreaBlocks, 1, "primary volume descriptor")

	// Walk the volume descriptor set and the volume recognition sequence that
	// may follow it.
	var svd []byte
	var brvd []byte
	lba := uint32(systemAreaBlocks + 1)
	terminated := false
	for ; lba < c.blocks; lba++ {
		d := c.block(lba)
		id := string(d[1:6])
		if terminated {
			// After the terminator only the UDF recognition sequence may
			// follow, and only directly.
			if id != "BEA01" && id != "NSR02" && id != "NSR03" && id != "TEA01" {
				break
			}
			c.claim(lba, 1, "volume recognition sequence "+id)
			continue
		}
		if id != "CD001" {
			c.t.Fatalf("block %d of the volume descriptor set has the standard identifier %q", lba, id)
		}
		switch d[0] {
		case 0:
			brvd = d
			c.claim(lba, 1, "boot record")
		case 2:
			svd = d
			c.claim(lba, 1, "supplementary volume descriptor")
			c.checkVolumeDescriptor(d, 2, true)
		case 255:
			terminated = true
			c.claim(lba, 1, "volume descriptor set terminator")
		default:
			c.t.Errorf("block %d holds an unexpected volume descriptor of type %d", lba, d[0])
		}
	}
	if !terminated {
		c.t.Error("the volume descriptor set has no terminator")
	}

	c.rockRidge = c.detectRockRidge(pvd)
	c.checkHierarchy(pvd, "primary", false)
	if svd != nil {
		c.checkHierarchy(svd, "joliet", true)
	}
	if brvd != nil {
		c.checkElTorito(brvd)
	}
	if udfPresent(c) {
		c.checkUDF()
	}
}

func readByte(t *testing.T, r io.ReaderAt, off int64) byte {
	var b [1]byte
	if _, err := r.ReadAt(b[:], off); err != nil && err != io.EOF {
		t.Fatalf("reading byte %d: %v", off, err)
	}
	return b[0]
}

// checkVolumeDescriptor checks the fields a volume descriptor shares between
// its primary and its supplementary form.
func (c *checker) checkVolumeDescriptor(d []byte, kind byte, joliet bool) {
	c.t.Helper()
	if d[0] != kind {
		c.t.Errorf("the volume descriptor has type %d, want %d", d[0], kind)
	}
	if got := string(d[1:6]); got != "CD001" {
		c.t.Errorf("the standard identifier is %q, want %q", got, "CD001")
	}
	if d[6] != 1 {
		c.t.Errorf("the volume descriptor version is %d, want 1", d[6])
	}
	c.checkBoth32(d[80:], "volume space size")
	c.checkBoth16(d[120:], "volume set size")
	c.checkBoth16(d[124:], "volume sequence number")
	if got := c.checkBoth16(d[128:], "logical block size"); got != BlockSize {
		c.t.Errorf("the logical block size is %d, want %d", got, BlockSize)
	}
	c.checkBoth32(d[132:], "path table size")
	if d[881] != 1 {
		c.t.Errorf("the file structure version is %d, want 1", d[881])
	}
	if joliet {
		if got := strings.TrimRight(string(d[88:120]), "\x00"); got != "%/E" {
			c.t.Errorf("the escape sequence is %q, want %q (UCS-2 level 3)", got, "%/E")
		}
	}
}

// checkBoth16 and checkBoth32 verify that a number recorded in both byte
// orders agrees with itself.
func (c *checker) checkBoth16(b []byte, what string) uint16 {
	c.t.Helper()
	le, be := binary.LittleEndian.Uint16(b), binary.BigEndian.Uint16(b[2:])
	if le != be {
		c.t.Errorf("%s is %d little endian but %d big endian", what, le, be)
	}
	return le
}

func (c *checker) checkBoth32(b []byte, what string) uint32 {
	c.t.Helper()
	le, be := binary.LittleEndian.Uint32(b), binary.BigEndian.Uint32(b[4:])
	if le != be {
		c.t.Errorf("%s is %d little endian but %d big endian", what, le, be)
	}
	return le
}

func (c *checker) detectRockRidge(pvd []byte) bool {
	root := pvd[156:190]
	block := c.block(binary.LittleEndian.Uint32(root[2:]))
	for _, e := range suspOf(block[:block[0]]) {
		if e.signature == "SP" {
			if len(e.payload) < 3 || e.payload[0] != 0xBE || e.payload[1] != 0xEF {
				c.t.Errorf("the SP entry has the payload %x, want the check bytes BE EF", e.payload)
			}
			return true
		}
	}
	return false
}

// checkHierarchy walks one directory tree, checking every record, and then
// checks that the path tables describe the same tree.
func (c *checker) checkHierarchy(vd []byte, name string, joliet bool) {
	c.t.Helper()
	root := vd[156:190]
	if root[0] != 34 {
		c.t.Errorf("%s: the root directory record is %d bytes, want 34", name, root[0])
	}
	if root[25]&flagDirectory == 0 {
		c.t.Errorf("%s: the root directory record is not marked as a directory", name)
	}
	rootLBA := c.checkBoth32(root[2:], name+" root extent")
	rootLength := c.checkBoth32(root[10:], name+" root length")

	// dirs collects the directories in the order the path table has to list
	// them: by level, then by parent, then by identifier. parent is the
	// one-based path table number of the parent; the root is its own parent.
	dirs := []checkedDir{{path: "", lba: rootLBA, length: rootLength, parent: 1}}
	for i := 0; i < len(dirs); i++ {
		d := dirs[i]
		c.claim(d.lba, d.length/BlockSize, fmt.Sprintf("%s directory %q", name, displayPath(d.path)))
		for _, rec := range c.records(d.lba, d.length, name, d.path, joliet) {
			if rec.name == "." {
				if rec.lba != d.lba || rec.length != d.length {
					c.t.Errorf("%s: the \".\" record of %q points at %d/%d, want %d/%d", name, displayPath(d.path), rec.lba, rec.length, d.lba, d.length)
				}
				continue
			}
			if rec.name == ".." {
				want := dirs[d.parent-1]
				if rec.lba != want.lba || rec.length != want.length {
					c.t.Errorf("%s: the \"..\" record of %q points at %d/%d, want %d/%d", name, displayPath(d.path), rec.lba, rec.length, want.lba, want.length)
				}
				continue
			}
			path := pathOf(d.path, rec.name)
			if rec.isDir {
				if rec.length == 0 || rec.length%BlockSize != 0 {
					c.t.Errorf("%s: the directory %q has an extent of %d bytes", name, path, rec.length)
					continue
				}
				dirs = append(dirs, checkedDir{path: path, lba: rec.lba, length: rec.length, parent: i + 1})
				continue
			}
			if rec.length > 0 {
				// Several names may share one extent, so file data is not
				// claimed exclusively; it only has to be inside the volume.
				if end := rec.lba + blocksFor(int64(rec.length)); end > c.blocks {
					c.t.Errorf("%s: the file %q ends at block %d, past the end of the %d block volume", name, path, end, c.blocks)
				}
			}
			if !joliet {
				c.entries[path] = &checkedEntry{path: path, lba: rec.lba, length: rec.length, susp: rec.susp}
			}
		}
	}
	c.checkPathTables(vd, name, dirs)
}

// checkedDir is a directory the checker found by walking the records.
type checkedDir struct {
	path        string
	lba, length uint32
	// parent is the one-based path table number of the parent directory.
	parent int
}

type checkedRecord struct {
	name        string
	isDir       bool
	lba, length uint32
	susp        []suspValue
	id          []byte
	// multiExtent reports that the file continues in the next record.
	multiExtent bool
}

// records parses the records of a directory extent, checking the invariants
// ECMA-119 puts on them.
func (c *checker) records(lba, length uint32, hierarchy, dirPath string, joliet bool) []checkedRecord {
	c.t.Helper()
	data := make([]byte, length)
	if err := readFull(c.r, data, int64(lba)*BlockSize); err != nil {
		c.t.Fatalf("reading the directory %q: %v", dirPath, err)
	}

	var out []checkedRecord
	for pos := 0; pos < len(data); {
		if pos%BlockSize > BlockSize-34 || data[pos] == 0 {
			for i := pos; i < pos+(BlockSize-pos%BlockSize); i++ {
				if data[i] != 0 {
					c.t.Errorf("%s: the padding of the directory %q is not zero at offset %d", hierarchy, displayPath(dirPath), i)
					break
				}
			}
			pos += BlockSize - pos%BlockSize
			continue
		}
		size := int(data[pos])
		if size%2 != 0 {
			c.t.Errorf("%s: a record of the directory %q is %d bytes, which is odd", hierarchy, displayPath(dirPath), size)
		}
		if pos%BlockSize+size > BlockSize {
			c.t.Fatalf("%s: a record of the directory %q crosses a block boundary at offset %d", hierarchy, displayPath(dirPath), pos)
		}
		rec := data[pos : pos+size]
		pos += size

		c.checkBoth32(rec[2:], "record extent")
		c.checkBoth32(rec[10:], "record length")
		c.checkBoth16(rec[28:], "volume sequence number")
		idLen := int(rec[32])
		if 33+idLen > size {
			c.t.Fatalf("%s: an identifier of %d bytes does not fit in a %d byte record", hierarchy, idLen, size)
		}
		id := rec[33 : 33+idLen]

		r := checkedRecord{
			isDir:       rec[25]&flagDirectory != 0,
			multiExtent: rec[25]&flagMultiExtent != 0,
			lba:         binary.LittleEndian.Uint32(rec[2:]),
			length:      binary.LittleEndian.Uint32(rec[10:]),
			id:          id,
		}
		switch {
		case idLen == 1 && id[0] == 0:
			r.name = "."
		case idLen == 1 && id[0] == 1:
			r.name = ".."
		case joliet:
			r.name = decodeUCS2(id)
		default:
			r.name = string(id)
			c.checkISOIdentifier(r.name, r.isDir, hierarchy, dirPath)
		}
		if !joliet && c.rockRidge {
			r.susp = c.suspWithContinuations(rec)
			c.checkRockRidgeEntries(r, hierarchy, dirPath)
			// Rock Ridge carries the real name, which is the one the UDF
			// hierarchy uses as well.
			if name := rockRidgeName(r.susp); name != "" {
				r.name = name
			}
		}
		out = append(out, r)
	}

	if len(out) < 2 || out[0].name != "." || out[1].name != ".." {
		c.t.Errorf("%s: the directory %q does not start with \".\" and \"..\"", hierarchy, displayPath(dirPath))
	}
	unit := identifierUnit(joliet)
	for i := 3; i < len(out); i++ {
		if out[i-1].multiExtent {
			// The records of one file share an identifier and are consecutive.
			if !bytes.Equal(out[i-1].id, out[i].id) {
				c.t.Errorf("%s: the directory %q has a record marked as continuing, but the next record is %q", hierarchy, displayPath(dirPath), out[i].name)
			}
			continue
		}
		if compareIdentifiers(out[i-1].id, out[i].id, unit) >= 0 {
			c.t.Errorf("%s: the directory %q is not sorted: %q comes before %q", hierarchy, displayPath(dirPath), out[i-1].name, out[i].name)
		}
	}
	c.checkMultiExtents(out, hierarchy, dirPath)
	return out
}

// checkMultiExtents checks the records of every file that spans more than one
// extent: the extents have to be contiguous, every one but the last has to be
// the largest a record can describe, and the last one has to end the run.
func (c *checker) checkMultiExtents(out []checkedRecord, hierarchy, dirPath string) {
	c.t.Helper()
	for i := 0; i < len(out); i++ {
		if !out[i].multiExtent {
			continue
		}
		first := i
		var total uint64
		for ; i < len(out); i++ {
			total += uint64(out[i].length)
			if i > first && out[i].lba != out[i-1].lba+out[i-1].length/BlockSize {
				c.t.Errorf("%s: %q: extent %d starts at block %d, but the previous one ends at %d",
					hierarchy, out[i].name, i-first, out[i].lba, out[i-1].lba+out[i-1].length/BlockSize)
			}
			if !out[i].multiExtent {
				break
			}
			if out[i].length != maxExtentLength {
				c.t.Errorf("%s: %q: extent %d is %d bytes, but every extent but the last has to be the %d a record can describe",
					hierarchy, out[i].name, i-first, out[i].length, int64(maxExtentLength))
			}
		}
		if i == len(out) {
			c.t.Errorf("%s: %q: the records end while the file is still marked as continuing", hierarchy, out[first].name)
			return
		}
		if total == 0 || total <= maxExtentLength {
			c.t.Errorf("%s: %q: %d bytes in %d extents, which would fit in one", hierarchy, out[first].name, total, i-first+1)
		}
	}
}

// checkISOIdentifier checks that an identifier of the primary hierarchy only
// uses the characters ISO 9660 allows and carries a version where it must.
func (c *checker) checkISOIdentifier(id string, isDir bool, hierarchy, dirPath string) {
	c.t.Helper()
	body := id
	if !isDir {
		i := strings.LastIndexByte(id, ';')
		if i < 0 {
			c.t.Errorf("%s: the file identifier %q in %q has no version", hierarchy, id, displayPath(dirPath))
			return
		}
		if id[i+1:] != "1" {
			c.t.Errorf("%s: the file identifier %q in %q has the version %q, want 1", hierarchy, id, displayPath(dirPath), id[i+1:])
		}
		body = id[:i]
	}
	limit := maxISOIdentifier
	if isDir {
		limit = maxDirIdentifier
	}
	if len(body) > limit {
		c.t.Errorf("%s: the identifier %q in %q is %d characters, more than the %d allowed", hierarchy, id, displayPath(dirPath), len(body), limit)
	}
	dots := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '.' {
			dots++
			continue
		}
		if !isDChar(body[i]) {
			c.t.Errorf("%s: the identifier %q in %q contains %q, which is not a d-character", hierarchy, id, displayPath(dirPath), body[i])
		}
	}
	if isDir && dots > 0 {
		c.t.Errorf("%s: the directory identifier %q in %q contains a dot", hierarchy, id, displayPath(dirPath))
	}
	if !isDir && dots > 1 {
		c.t.Errorf("%s: the file identifier %q in %q contains %d dots, want at most one", hierarchy, id, displayPath(dirPath), dots)
	}
}

// suspWithContinuations reads the system use entries of a record, following
// the continuation areas.
func (c *checker) suspWithContinuations(rec []byte) []suspValue {
	c.t.Helper()
	entries := suspOf(rec)
	var out []suspValue
	for depth := 0; depth < 8; depth++ {
		var next *ceRef
		for _, e := range entries {
			if e.signature == "CE" {
				if len(e.payload) != 24 {
					c.t.Errorf("a CE entry has a payload of %d bytes, want 24", len(e.payload))
					continue
				}
				c.checkBoth32(e.payload[0:], "continuation area block")
				c.checkBoth32(e.payload[8:], "continuation area offset")
				c.checkBoth32(e.payload[16:], "continuation area length")
				next = &ceRef{
					block:  binary.LittleEndian.Uint32(e.payload[0:]),
					offset: binary.LittleEndian.Uint32(e.payload[8:]),
					length: binary.LittleEndian.Uint32(e.payload[16:]),
				}
				continue
			}
			out = append(out, e)
		}
		if next == nil {
			return out
		}
		if next.offset+next.length > BlockSize {
			c.t.Errorf("a continuation area at %d+%d of %d bytes crosses a block boundary", next.block, next.offset, next.length)
			return out
		}
		area := make([]byte, next.length)
		if err := readFull(c.r, area, int64(next.block)*BlockSize+int64(next.offset)); err != nil {
			c.t.Fatalf("reading a continuation area: %v", err)
		}
		if got := suspIn(area); suspSizeOf(got) != int(next.length) {
			c.t.Errorf("a continuation area of %d bytes holds entries worth %d bytes", next.length, suspSizeOf(got))
		}
		entries = suspIn(area)
	}
	c.t.Error("the continuation areas of a record form a chain that is too long")
	return out
}

func suspSizeOf(entries []suspValue) int {
	total := 0
	for _, e := range entries {
		total += 4 + len(e.payload)
	}
	return total
}

// rockRidgeName assembles the real name out of the NM entries of a record.
func rockRidgeName(entries []suspValue) string {
	name := ""
	for _, e := range entries {
		if e.signature != "NM" || len(e.payload) < 1 {
			continue
		}
		if e.payload[0]&(nmCurrent|nmParent) != 0 {
			continue
		}
		name += string(e.payload[1:])
	}
	return name
}

// checkRockRidgeEntries checks that every record carries the entries Rock
// Ridge requires and that they are well formed.
func (c *checker) checkRockRidgeEntries(r checkedRecord, hierarchy, dirPath string) {
	c.t.Helper()
	seen := map[string]int{}
	for _, e := range r.susp {
		seen[e.signature]++
	}
	if seen["PX"] != 1 {
		c.t.Errorf("%s: the record for %q in %q has %d PX entries, want one", hierarchy, r.name, displayPath(dirPath), seen["PX"])
	}
	if seen["TF"] != 1 {
		c.t.Errorf("%s: the record for %q in %q has %d TF entries, want one", hierarchy, r.name, displayPath(dirPath), seen["TF"])
	}
	if r.name != "." && r.name != ".." && seen["NM"] == 0 {
		c.t.Errorf("%s: the record for %q in %q has no NM entry", hierarchy, r.name, displayPath(dirPath))
	}
	for _, e := range r.susp {
		switch e.signature {
		case "PX":
			if len(e.payload) != 32 {
				c.t.Errorf("a PX entry has a payload of %d bytes, want 32", len(e.payload))
				continue
			}
			for i, what := range []string{"mode", "link count", "uid", "gid"} {
				c.checkBoth32(e.payload[8*i:], "PX "+what)
			}
		case "PN":
			if len(e.payload) != 16 {
				c.t.Errorf("a PN entry has a payload of %d bytes, want 16", len(e.payload))
			}
		case "TF":
			if len(e.payload) < 1 {
				c.t.Error("a TF entry has an empty payload")
				continue
			}
			want := 1
			for bit := 0; bit < 7; bit++ {
				if e.payload[0]&(1<<bit) != 0 {
					want += 7
				}
			}
			if len(e.payload) != want {
				c.t.Errorf("a TF entry with flags %#x has a payload of %d bytes, want %d", e.payload[0], len(e.payload), want)
			}
		}
	}
}

// checkPathTables checks that both path tables describe the directories the
// records do, in the order ECMA-119 requires.
func (c *checker) checkPathTables(vd []byte, name string, dirs []checkedDir) {
	c.t.Helper()
	size := binary.LittleEndian.Uint32(vd[132:])
	lTable := binary.LittleEndian.Uint32(vd[140:])
	mTable := binary.BigEndian.Uint32(vd[148:])
	c.claim(lTable, blocksFor(int64(size)), name+" type L path table")
	c.claim(mTable, blocksFor(int64(size)), name+" type M path table")

	for _, table := range []struct {
		lba       uint32
		bigEndian bool
	}{{lTable, false}, {mTable, true}} {
		data := make([]byte, uint64(blocksFor(int64(size)))*BlockSize)
		if err := readFull(c.r, data, int64(table.lba)*BlockSize); err != nil {
			c.t.Fatalf("reading a path table: %v", err)
		}
		pos := 0
		for i, dir := range dirs {
			if pos+8 > int(size) {
				c.t.Fatalf("%s: the path table ends after %d of %d records", name, i, len(dirs))
			}
			idLen := int(data[pos])
			var lba uint32
			var parent uint16
			if table.bigEndian {
				lba = binary.BigEndian.Uint32(data[pos+2:])
				parent = binary.BigEndian.Uint16(data[pos+6:])
			} else {
				lba = binary.LittleEndian.Uint32(data[pos+2:])
				parent = binary.LittleEndian.Uint16(data[pos+6:])
			}
			if lba != dir.lba {
				c.t.Errorf("%s: path table record %d points at block %d, want %d", name, i+1, lba, dir.lba)
			}
			if int(parent) != dir.parent {
				c.t.Errorf("%s: path table record %d has parent %d, want %d", name, i+1, parent, dir.parent)
			}
			pos += 8 + idLen
			if idLen%2 != 0 {
				pos++
			}
		}
		if pos != int(size) {
			c.t.Errorf("%s: the path table holds %d bytes of records but declares %d", name, pos, size)
		}
	}
}

// checkElTorito checks the boot record and the boot catalog.
func (c *checker) checkElTorito(brvd []byte) {
	c.t.Helper()
	if got := strings.TrimRight(string(brvd[7:39]), "\x00"); got != "EL TORITO SPECIFICATION" {
		c.t.Errorf("the boot system identifier is %q, want %q", got, "EL TORITO SPECIFICATION")
	}
	catalogLBA := binary.LittleEndian.Uint32(brvd[71:])
	c.claim(catalogLBA, 1, "boot catalog")
	catalog := c.block(catalogLBA)

	if catalog[0] != 1 {
		c.t.Errorf("the validation entry has the header id %d, want 1", catalog[0])
	}
	if catalog[30] != 0x55 || catalog[31] != 0xAA {
		c.t.Errorf("the validation entry ends with %02x %02x, want 55 AA", catalog[30], catalog[31])
	}
	var sum uint16
	for i := 0; i < 32; i += 2 {
		sum += binary.LittleEndian.Uint16(catalog[i:])
	}
	if sum != 0 {
		c.t.Errorf("the words of the validation entry add up to %#x, want zero", sum)
	}
	if catalog[32] != bootIndicatorBootable {
		c.t.Errorf("the default boot entry has the indicator %#x, want %#x", catalog[32], bootIndicatorBootable)
	}
	// Every further entry is a section header followed by one section entry.
	for pos := 64; pos < BlockSize && catalog[pos] != 0; pos += 64 {
		header := catalog[pos : pos+32]
		if header[0] != sectionHeaderMore && header[0] != sectionHeaderFinal {
			c.t.Errorf("the entry at offset %d has the indicator %#x, want a section header", pos, header[0])
			break
		}
		if got := binary.LittleEndian.Uint16(header[2:]); got != 1 {
			c.t.Errorf("the section header at offset %d declares %d entries, want one", pos, got)
		}
		if catalog[pos+32] != bootIndicatorBootable {
			c.t.Errorf("the section entry at offset %d has the indicator %#x, want %#x", pos+32, catalog[pos+32], bootIndicatorBootable)
		}
	}
}

// bootImageLBAs returns the block every boot entry of the catalog points at,
// in catalog order.
func (c *checker) bootImageLBAs() []uint32 {
	c.t.Helper()
	pvd := c.block(systemAreaBlocks)
	_ = pvd
	var catalogLBA uint32
	for lba := uint32(systemAreaBlocks); lba < c.blocks; lba++ {
		d := c.block(lba)
		if string(d[1:6]) != "CD001" {
			break
		}
		if d[0] == 0 {
			catalogLBA = binary.LittleEndian.Uint32(d[71:])
			break
		}
		if d[0] == 255 {
			break
		}
	}
	if catalogLBA == 0 {
		return nil
	}
	catalog := c.block(catalogLBA)
	out := []uint32{binary.LittleEndian.Uint32(catalog[32+8:])}
	for pos := 64; pos+64 <= BlockSize && catalog[pos] != 0; pos += 64 {
		out = append(out, binary.LittleEndian.Uint32(catalog[pos+32+8:]))
	}
	return out
}

// Helpers shared with the other test files.

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

func decodeUCS2(b []byte) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(units))
}

// jolietView reads the Joliet hierarchy of an image, which the Reader does not
// look at.
type jolietView struct {
	r                   io.ReaderAt
	rootLBA, rootLength uint32
}

func openJoliet(img *memImage) (*jolietView, error) {
	for lba := uint32(systemAreaBlocks); ; lba++ {
		b := make([]byte, BlockSize)
		if err := readFull(img, b, int64(lba)*BlockSize); err != nil {
			return nil, err
		}
		if string(b[1:6]) != "CD001" {
			return nil, fmt.Errorf("no supplementary volume descriptor before block %d", lba)
		}
		if b[0] == 255 {
			return nil, fmt.Errorf("the image has no supplementary volume descriptor")
		}
		if b[0] != 2 {
			continue
		}
		return &jolietView{
			r:          img,
			rootLBA:    binary.LittleEndian.Uint32(b[158:]),
			rootLength: binary.LittleEndian.Uint32(b[166:]),
		}, nil
	}
}

// names collects the paths of the Joliet hierarchy.
func (j *jolietView) names(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var walk func(lba, length uint32, dirPath string)
	walk = func(lba, length uint32, dirPath string) {
		data := make([]byte, length)
		if err := readFull(j.r, data, int64(lba)*BlockSize); err != nil {
			t.Fatalf("reading a Joliet directory: %v", err)
		}
		for pos := 0; pos < len(data); {
			if pos%BlockSize > BlockSize-34 || data[pos] == 0 {
				pos += BlockSize - pos%BlockSize
				continue
			}
			rec := data[pos : pos+int(data[pos])]
			pos += int(data[pos])
			idLen := int(rec[32])
			id := rec[33 : 33+idLen]
			if idLen == 1 && (id[0] == 0 || id[0] == 1) {
				continue
			}
			name := decodeUCS2(id)
			path := pathOf(dirPath, name)
			out[path] = true
			if rec[25]&flagDirectory != 0 {
				walk(binary.LittleEndian.Uint32(rec[2:]), binary.LittleEndian.Uint32(rec[10:]), path)
			}
		}
	}
	walk(j.rootLBA, j.rootLength, "")
	return out
}
