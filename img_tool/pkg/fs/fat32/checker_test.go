package fat32

import (
	"encoding/binary"
	"io"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"
)

// This file contains an independent reader and checker for the images the
// package writes. It only relies on the on-disk format (the Microsoft FAT
// specification), not on the writer's bookkeeping, so it catches layout
// mistakes the way fsck would.

type vfile struct {
	path     string
	dir      bool
	short    string
	attr     byte
	size     uint32
	first    uint32
	data     []byte
	chain    []uint32
	mdate    uint16
	mtime    uint16
	adate    uint16
	tenth    byte
	hasLong  bool
	children []string
}

// contiguous reports whether the cluster chain is one run.
func (f *vfile) contiguous() bool {
	for i := 1; i < len(f.chain); i++ {
		if f.chain[i] != f.chain[i-1]+1 {
			return false
		}
	}
	return true
}

type checker struct {
	t *testing.T
	r io.ReaderAt

	ss, spc, reserved, fatSz, total uint32
	clusters                        uint32
	dataStart                       uint32
	clusterBytes                    int
	boot                            []byte
	fat                             []byte // the first FAT
	label                           string

	files    map[string]*vfile
	used     map[uint32]string
	usedList []uint32
}

func (c *checker) read(off int64, n int) []byte {
	c.t.Helper()
	b := make([]byte, n)
	if _, err := c.r.ReadAt(b, off); err != nil && err != io.EOF {
		c.t.Fatalf("reading %d bytes at %d: %v", n, off, err)
	}
	return b
}

func (c *checker) errorf(format string, args ...any) {
	c.t.Helper()
	c.t.Errorf(format, args...)
}

func (c *checker) fatEntry(cluster uint32) uint32 {
	return binary.LittleEndian.Uint32(c.fat[cluster*4:]) & 0x0FFFFFFF
}

func (c *checker) clusterOffset(cluster uint32) int64 {
	return (int64(c.dataStart) + int64(cluster-2)*int64(c.spc)) * int64(c.ss)
}

// checkImage verifies the whole image and returns the files by path.
func checkImage(t *testing.T, r io.ReaderAt, size int64) *checker {
	t.Helper()
	c := &checker{t: t, r: r, files: map[string]*vfile{}, used: map[uint32]string{}}
	c.parseBoot(size)
	c.parseFAT()
	c.walk(rootCluster, "", 0)
	c.checkAllocation()
	c.checkFSInfo()
	return c
}

func (c *checker) parseBoot(size int64) {
	le := binary.LittleEndian
	c.boot = c.read(0, 512)
	b := c.boot
	if b[510] != 0x55 || b[511] != 0xAA {
		c.t.Fatalf("missing boot signature")
	}
	if !(b[0] == 0xEB && b[2] == 0x90) && b[0] != 0xE9 {
		c.errorf("bad jump instruction % x", b[:3])
	}
	c.ss = uint32(le.Uint16(b[11:]))
	c.spc = uint32(b[13])
	c.reserved = uint32(le.Uint16(b[14:]))
	c.fatSz = le.Uint32(b[36:])
	c.total = le.Uint32(b[32:])
	c.clusterBytes = int(c.ss * c.spc)
	switch c.ss {
	case 512, 1024, 2048, 4096:
	default:
		c.t.Fatalf("bad sector size %d", c.ss)
	}
	if c.spc == 0 || c.spc&(c.spc-1) != 0 || c.spc > 128 {
		c.t.Fatalf("bad sectors per cluster %d", c.spc)
	}
	if c.reserved < 8 {
		c.errorf("reserved sectors %d", c.reserved)
	}
	if b[16] != 2 || le.Uint16(b[17:]) != 0 || le.Uint16(b[19:]) != 0 || le.Uint16(b[22:]) != 0 {
		c.errorf("bad FAT count, root entries, total sectors 16 or FAT size 16")
	}
	if b[21] != 0xF8 {
		c.errorf("media %#x", b[21])
	}
	if le.Uint16(b[40:]) != 0 || le.Uint16(b[42:]) != 0 {
		c.errorf("extended flags or version set")
	}
	if le.Uint32(b[44:]) != 2 || le.Uint16(b[48:]) != 1 || le.Uint16(b[50:]) != 6 {
		c.errorf("root cluster, FSInfo or backup sector wrong")
	}
	for _, v := range b[52:64] {
		if v != 0 {
			c.errorf("reserved bytes are not zero")
		}
	}
	if b[66] != 0x29 || string(b[82:90]) != "FAT32   " {
		c.errorf("extended boot signature or file system type")
	}
	if int64(c.total)*int64(c.ss) != size {
		c.errorf("image is %d bytes, boot sector says %d sectors of %d", size, c.total, c.ss)
	}
	c.dataStart = c.reserved + 2*c.fatSz
	c.clusters = (c.total - c.dataStart) / c.spc
	if c.clusters < 65525 {
		c.errorf("only %d clusters: not a FAT32 volume", c.clusters)
	}
	if c.fatSz*c.ss < (c.clusters+2)*4 {
		c.errorf("FAT of %d sectors is too small for %d clusters", c.fatSz, c.clusters)
	}
	if c.dataStart%c.spc != 0 {
		c.errorf("data region at sector %d is not aligned to the cluster size", c.dataStart)
	}
	c.label = strings.TrimRight(string(b[71:82]), " ")

	// the backup boot sectors
	for i := 0; i < 3; i++ {
		orig := c.read(int64(i)*int64(c.ss), int(c.ss))
		backup := c.read(int64(6+i)*int64(c.ss), int(c.ss))
		if string(orig) != string(backup) {
			c.errorf("backup of boot sector %d differs", i)
		}
	}
}

func (c *checker) parseFAT() {
	le := binary.LittleEndian
	n := int((c.clusters + 2) * 4)
	c.fat = c.read(int64(c.reserved)*int64(c.ss), n)
	second := c.read(int64(c.reserved+c.fatSz)*int64(c.ss), n)
	if string(c.fat) != string(second) {
		c.errorf("the two FATs differ")
	}
	if got := le.Uint32(c.fat[0:]); got != 0x0FFFFFF8 {
		c.errorf("FAT[0] = %#x", got)
	}
	if got := le.Uint32(c.fat[4:]); got != 0x0FFFFFFF {
		c.errorf("FAT[1] = %#x", got)
	}
	// Sectors of the FAT past the last entry are zero.
	rest := c.read(int64(c.reserved)*int64(c.ss)+int64(n), int(c.fatSz*c.ss)-n)
	for _, v := range rest {
		if v != 0 {
			c.errorf("the FAT has data past the last entry")
			break
		}
	}
	for cl := uint32(0); cl < c.clusters+2; cl++ {
		if le.Uint32(c.fat[cl*4:])>>28 != 0 {
			c.errorf("FAT[%d] has the reserved high bits set", cl)
			break
		}
	}
}

// chain follows the cluster chain from first, verifying it.
func (c *checker) chain(owner string, first uint32) []uint32 {
	if first == 0 {
		return nil
	}
	var chain []uint32
	for cl := first; ; {
		if cl < 2 || cl >= c.clusters+2 {
			c.t.Fatalf("%s: cluster %d out of range", owner, cl)
		}
		if prev, dup := c.used[cl]; dup {
			c.errorf("%s: cluster %d already belongs to %s", owner, cl, prev)
			return chain
		}
		c.used[cl] = owner
		chain = append(chain, cl)
		next := c.fatEntry(cl)
		if next >= 0x0FFFFFF8 {
			return chain
		}
		if next < 2 {
			c.t.Fatalf("%s: chain continues with %#x after cluster %d", owner, next, cl)
		}
		cl = next
	}
}

func (c *checker) readChain(chain []uint32) []byte {
	var out []byte
	for _, cl := range chain {
		out = append(out, c.read(c.clusterOffset(cl), c.clusterBytes)...)
	}
	return out
}

func validShortChars(s string) bool {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == ' ' {
			continue
		}
		if !isShortChar(ch) && ch != '.' {
			return false
		}
	}
	return true
}

// walk parses the directory at the given cluster.
func (c *checker) walk(cluster uint32, dirPath string, parentCluster uint32) {
	le := binary.LittleEndian
	owner := dirPath
	if owner == "" {
		owner = "/"
	}
	chain := c.chain(owner, cluster)
	data := c.readChain(chain)
	self := &vfile{path: dirPath, dir: true, first: cluster, chain: chain}
	c.files[dirPath] = self

	seen := map[string]string{}
	var lfnParts [][32]byte // the long name entries collected for the next short entry
	ended := false
	for off := 0; off+32 <= len(data); off += 32 {
		d := data[off : off+32]
		switch {
		case d[0] == 0x00:
			ended = true
			continue
		case ended:
			c.errorf("%s: entry after the end marker", owner)
			continue
		case d[0] == 0xE5:
			continue
		}
		if d[11]&0x3F == 0x0F { // long name entry
			lfnParts = append(lfnParts, [32]byte(d))
			continue
		}
		if d[11]&attrVolumeID != 0 {
			if dirPath != "" {
				c.errorf("%s: volume label entry in a subdirectory", owner)
			}
			if label := strings.TrimRight(string(d[:11]), " "); label != c.label {
				c.errorf("volume label entry %q differs from the boot sector %q", label, c.label)
			}
			if le.Uint16(d[20:]) != 0 || le.Uint16(d[26:]) != 0 {
				c.errorf("volume label has a cluster")
			}
			lfnParts = nil
			continue
		}

		var short [11]byte
		copy(short[:], d[:11])
		first := uint32(le.Uint16(d[20:]))<<16 | uint32(le.Uint16(d[26:]))
		size := le.Uint32(d[28:])
		attr := d[11]
		if attr&0xC0 != 0 || d[12] != 0 {
			c.errorf("%s: reserved bits set in the entry %q", owner, short[:])
		}

		// the special entries
		if off < 64 && dirPath != "" && (short[0] == '.') {
			switch off {
			case 0:
				if string(short[:]) != ".          " || first != cluster || attr&attrDirectory == 0 || size != 0 {
					c.errorf("%s: bad '.' entry", owner)
				}
			case 32:
				if string(short[:]) != "..         " || first != parentCluster || attr&attrDirectory == 0 || size != 0 {
					c.errorf("%s: bad '..' entry (cluster %d, want %d)", owner, first, parentCluster)
				}
			}
			continue
		}

		if short[0] == 0x20 || !validShortChars(string(short[:])) || strings.ToUpper(string(short[:])) != string(short[:]) {
			c.errorf("%s: invalid short name %q", owner, short[:])
		}
		name := strings.TrimRight(string(short[:8]), " ")
		if ext := strings.TrimRight(string(short[8:]), " "); ext != "" {
			name += "." + ext
		}
		hasLong := false
		if len(lfnParts) > 0 {
			hasLong = true
			name = c.assembleLong(owner, lfnParts, short)
		}
		lfnParts = nil

		if prev, dup := seen[strings.ToUpper(name)]; dup {
			c.errorf("%s: %q and %q are the same name", owner, name, prev)
		}
		seen[strings.ToUpper(name)] = name
		shortName := strings.TrimRight(string(short[:8]), " ")
		if ext := strings.TrimRight(string(short[8:]), " "); ext != "" {
			shortName += "." + ext
		}
		if shortName != name {
			if prev, dup := seen[shortName]; dup && prev != name {
				c.errorf("%s: short name %q clashes with %q", owner, shortName, prev)
			}
			seen[shortName] = name
		}

		childPath := name
		if dirPath != "" {
			childPath = dirPath + "/" + name
		}
		f := &vfile{
			path: childPath, short: shortName, attr: attr, size: size, first: first, hasLong: hasLong,
			dir: attr&attrDirectory != 0, tenth: d[13],
			mtime: le.Uint16(d[22:]), mdate: le.Uint16(d[24:]), adate: le.Uint16(d[18:]),
		}
		if le.Uint16(d[14:]) != f.mtime || le.Uint16(d[16:]) != f.mdate {
			c.errorf("%s: creation time differs from the modification time", childPath)
		}
		self.children = append(self.children, name)
		if f.dir {
			if size != 0 {
				c.errorf("%s: directory with size %d", childPath, size)
			}
			c.walk(first, childPath, parentFor(cluster, dirPath))
			// keep the attributes the recursive call did not know about
			sub := c.files[childPath]
			sub.short, sub.attr, sub.mtime, sub.mdate, sub.adate, sub.tenth, sub.hasLong = f.short, f.attr, f.mtime, f.mdate, f.adate, f.tenth, f.hasLong
			continue
		}
		f.chain = c.chain(childPath, first)
		wantClusters := (int(size) + c.clusterBytes - 1) / c.clusterBytes
		if len(f.chain) != wantClusters {
			c.errorf("%s: %d clusters for %d bytes", childPath, len(f.chain), size)
		}
		if (size == 0) != (first == 0) {
			c.errorf("%s: first cluster %d for size %d", childPath, first, size)
		}
		f.data = c.readChain(f.chain)[:size]
		c.files[childPath] = f
	}
	if len(lfnParts) > 0 {
		c.errorf("%s: long name entries without a short entry", owner)
	}
	sort.Strings(self.children)
}

// parentFor is the cluster a subdirectory records for its parent: zero if the
// parent is the root.
func parentFor(cluster uint32, dirPath string) uint32 {
	if dirPath == "" {
		return 0
	}
	return cluster
}

func (c *checker) assembleLong(owner string, parts [][32]byte, short [11]byte) string {
	checksum := byte(0)
	for _, ch := range short {
		checksum = ((checksum&1)<<7 | checksum>>1) + ch
	}
	n := len(parts)
	if parts[0][0] != byte(n)|0x40 {
		c.errorf("%s: first long name entry has the ordinal %#x, want %#x", owner, parts[0][0], byte(n)|0x40)
	}
	var units []uint16
	for i, p := range parts {
		wantOrd := byte(n - i)
		if i == 0 {
			wantOrd |= 0x40
		}
		if p[0] != wantOrd || p[11] != 0x0F || p[12] != 0 || p[26] != 0 || p[27] != 0 {
			c.errorf("%s: bad long name entry %d", owner, i)
		}
		if p[13] != checksum {
			c.errorf("%s: long name checksum %#x, want %#x", owner, p[13], checksum)
		}
		var chunk []uint16
		for _, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
			chunk = append(chunk, binary.LittleEndian.Uint16(p[off:]))
		}
		units = append(chunk, units...)
	}
	// terminator and padding
	end := len(units)
	for i, u := range units {
		if u == 0 {
			end = i
			for _, pad := range units[i+1:] {
				if pad != 0xFFFF {
					c.errorf("%s: long name padding is %#x", owner, pad)
				}
			}
			break
		}
	}
	if len(units) > end && len(units)-13 >= end+1 && n > 1 {
		c.errorf("%s: unneeded long name entries", owner)
	}
	return string(utf16.Decode(units[:end]))
}

func (c *checker) checkAllocation() {
	// every FAT entry that is not free belongs to a chain we walked, and the reverse
	for cl := uint32(2); cl < c.clusters+2; cl++ {
		_, owned := c.used[cl]
		if (c.fatEntry(cl) != 0) != owned {
			c.errorf("cluster %d: FAT entry %#x, owned: %v", cl, c.fatEntry(cl), owned)
			return
		}
	}
	c.usedList = c.usedList[:0]
	for cl := range c.used {
		c.usedList = append(c.usedList, cl)
	}
}

func (c *checker) checkFSInfo() {
	le := binary.LittleEndian
	info := c.read(int64(c.ss), int(c.ss))
	if le.Uint32(info[0:]) != 0x41615252 || le.Uint32(info[484:]) != 0x61417272 || le.Uint32(info[508:]) != 0xAA550000 {
		c.errorf("bad FSInfo signatures")
	}
	for _, v := range info[4:484] {
		if v != 0 {
			c.errorf("FSInfo reserved bytes are not zero")
			break
		}
	}
	free := le.Uint32(info[488:])
	if want := c.clusters - uint32(len(c.used)); free != want {
		c.errorf("FSInfo free count %d, want %d", free, want)
	}
	if next := le.Uint32(info[492:]); free > 0 && c.fatEntry(next) != 0 {
		c.errorf("FSInfo next free cluster %d is not free", next)
	}
}

// memFile is an in-memory WriterAt/ReaderAt that reads zeros for ranges that
// were never written.
type memFile struct {
	b      []byte
	writes int
	bytes  int
}

func (m *memFile) grow(n int64) {
	if int64(len(m.b)) < n {
		m.b = append(m.b, make([]byte, n-int64(len(m.b)))...)
	}
}

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	m.grow(off + int64(len(p)))
	copy(m.b[off:], p)
	m.writes++
	m.bytes += len(p)
	return len(p), nil
}

func (m *memFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (m *memFile) Truncate(size int64) error {
	m.grow(size)
	m.b = m.b[:size]
	return nil
}

// noTruncate hides Truncate to exercise the fallback for plain WriterAts.
type noTruncate struct{ m *memFile }

func (n noTruncate) WriteAt(p []byte, off int64) (int, error) { return n.m.WriteAt(p, off) }

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}
