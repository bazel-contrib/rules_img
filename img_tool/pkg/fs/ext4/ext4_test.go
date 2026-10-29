package ext4

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func mustBuilder(t *testing.T, opts Options) *Builder {
	t.Helper()
	b, err := NewBuilder(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func addFile(t *testing.T, b *Builder, name string, content []byte, mode int64) {
	t.Helper()
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: mode}
	if err := b.WriteRegular(hdr, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
}

func addDir(t *testing.T, b *Builder, name string, mode int64) {
	t.Helper()
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: mode}); err != nil {
		t.Fatal(err)
	}
}

func build(t *testing.T, b *Builder) (*memFile, *Info) {
	t.Helper()
	m := &memFile{}
	info, err := b.Build(m)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(m.b)) != info.Size {
		t.Fatalf("image is %d bytes, info says %d", len(m.b), info.Size)
	}
	return m, info
}

// populate fills a builder with a representative tree and returns the
// expected content of the regular files.
func populate(t *testing.T, b *Builder) map[string][]byte {
	t.Helper()
	files := map[string][]byte{
		"etc/hostname":       []byte("example\n"),
		"etc/empty":          {},
		"bin/one-block":      pattern(4096, 1),
		"bin/odd":            pattern(5000, 2),
		"usr/lib/big":        pattern(3<<20+123, 3),
		"usr/lib/exactly-2k": pattern(2048, 4),
		"zeros":              make([]byte, 100000),
	}
	must := func(hdr *tar.Header) {
		t.Helper()
		if err := b.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
	}
	// Attributes that do not fit the inode and live in a block of their own.
	must(&tar.Header{Typeflag: tar.TypeDir, Name: "etc", Mode: 0o750, PAXRecords: map[string]string{
		"SCHILY.xattr.user.big":   strings.Repeat("v", 300),
		"SCHILY.xattr.user.other": "o",
	}})
	for _, name := range []string{"etc/hostname", "etc/empty", "bin/one-block", "bin/odd", "usr/lib/big", "usr/lib/exactly-2k", "zeros"} {
		addFile(t, b, name, files[name], 0o644)
	}
	must(&tar.Header{Typeflag: tar.TypeSymlink, Name: "etc/short-link", Linkname: "hostname"})
	must(&tar.Header{Typeflag: tar.TypeSymlink, Name: "etc/long-link", Linkname: "/" + strings.Repeat("a/", 40) + "z"})
	must(&tar.Header{Typeflag: tar.TypeLink, Name: "bin/hard", Linkname: "bin/odd"})
	// A label in the inode, on an entry that has no data blocks of its own.
	must(&tar.Header{Typeflag: tar.TypeChar, Name: "dev/null", Devmajor: 1, Devminor: 3, Mode: 0o666, PAXRecords: map[string]string{
		"SCHILY.xattr.security.selinux": "system_u:object_r:null_device_t:s0\x00",
	}})
	must(&tar.Header{Typeflag: tar.TypeBlock, Name: "dev/big", Devmajor: 259, Devminor: 70000, Mode: 0o660})
	must(&tar.Header{Typeflag: tar.TypeFifo, Name: "run/fifo", Mode: 0o600})
	// Both kinds of POSIX ACL, which are rewritten into the on-disk form.
	must(&tar.Header{Typeflag: tar.TypeDir, Name: "tmp", Mode: 0o1777, PAXRecords: map[string]string{
		"SCHILY.xattr.system.posix_acl_access": string(aclXattr(
			[3]uint32{0x01, 0o7, noACLID}, // user_obj
			[3]uint32{0x02, 0o5, 1000},    // user:1000
			[3]uint32{0x04, 0o5, noACLID}, // group_obj
			[3]uint32{0x08, 0o4, 100},     // group:100
			[3]uint32{0x10, 0o7, noACLID}, // mask
			[3]uint32{0x20, 0o0, noACLID}, // other
		)),
		"SCHILY.xattr.system.posix_acl_default": string(aclXattr(
			[3]uint32{0x01, 0o7, noACLID},
			[3]uint32{0x04, 0o5, noACLID},
			[3]uint32{0x20, 0o5, noACLID},
		)),
	}})
	return files
}

const noACLID = 0xFFFFFFFF

// aclXattr builds a POSIX ACL the way getxattr reports it (version 2, with
// tag, permissions and id in eight bytes per entry).
func aclXattr(entries ...[3]uint32) []byte {
	out := binary.LittleEndian.AppendUint32(nil, 2)
	for _, e := range entries {
		out = binary.LittleEndian.AppendUint16(out, uint16(e[0]))
		out = binary.LittleEndian.AppendUint16(out, uint16(e[1]))
		out = binary.LittleEndian.AppendUint32(out, e[2])
	}
	return out
}

func TestBuildTree(t *testing.T) {
	for _, bs := range []int{1024, 2048, 4096} {
		t.Run("", func(t *testing.T) {
			b := mustBuilder(t, Options{BlockSize: bs})
			files := populate(t, b)
			m, info := build(t, b)

			c := checkImage(t, m, info.Size)
			if t.Failed() {
				return
			}

			for name, want := range files {
				n := c.nodes[name]
				if n == nil {
					t.Fatalf("%s: missing", name)
				}
				if !bytes.Equal(n.data, want) {
					t.Errorf("%s: wrong content", name)
				}
				if !n.contiguous {
					t.Errorf("%s: not contiguous: %v", name, n.extents)
				}
			}
			// Files are readable straight from the image.
			for _, f := range info.Files {
				name := f.Path
				if name == "bin/hard" {
					name = "bin/odd" // the same inode, listed under its first path
				}
				if got := m.b[f.Offset : f.Offset+f.Size]; !bytes.Equal(got, files[name]) {
					t.Errorf("%s: image bytes at %d differ", f.Path, f.Offset)
				}
			}
			if len(info.Files) != len(files)-1 { // the empty file has no data
				t.Errorf("got %d file layouts, want %d", len(info.Files), len(files)-1)
			}

			if n := c.nodes["etc/short-link"]; n.target != "hostname" || n.flags&flagExtents != 0 {
				t.Errorf("short symlink: %q flags %#x", n.target, n.flags)
			}
			if n := c.nodes["etc/long-link"]; n.target != "/"+strings.Repeat("a/", 40)+"z" || n.flags&flagExtents == 0 {
				t.Errorf("long symlink: %q flags %#x", n.target, n.flags)
			}
			if c.nodes["bin/hard"].ino != c.nodes["bin/odd"].ino || c.nodes["bin/odd"].links != 2 {
				t.Errorf("hard link: inodes %d %d, links %d", c.nodes["bin/hard"].ino, c.nodes["bin/odd"].ino, c.nodes["bin/odd"].links)
			}
			if n := c.nodes["dev/null"]; n.mode != 0x2000|0o666 || n.dev != 1<<20|3 {
				t.Errorf("dev/null: mode %#o dev %#x", n.mode, n.dev)
			}
			if n := c.nodes["dev/big"]; n.mode != 0x6000|0o660 || n.dev != 259<<20|70000 {
				t.Errorf("dev/big: mode %#o dev %#x", n.mode, n.dev)
			}
			if n := c.nodes["tmp"]; n.mode != 0x4000|0o1777 {
				t.Errorf("tmp: mode %#o", n.mode)
			}
			if n := c.nodes["etc"]; n.mode != 0x4000|0o750 {
				t.Errorf("etc: mode %#o", n.mode)
			}
			if _, ok := c.nodes["lost+found"]; !ok {
				t.Error("lost+found missing")
			}
			if c.nodes["lost+found"].ino != firstIno {
				t.Errorf("lost+found is inode %d", c.nodes["lost+found"].ino)
			}
		})
	}
}

func TestDeterministicAndOrder(t *testing.T) {
	var images [][]byte
	var infos []*Info
	for i := 0; i < 2; i++ {
		b := mustBuilder(t, Options{})
		populate(t, b)
		m, info := build(t, b)
		images = append(images, m.b)
		infos = append(infos, info)
	}
	if !bytes.Equal(images[0], images[1]) {
		t.Error("two builds of the same input differ")
	}

	// Data follows the order the files were added in.
	var last int64 = -1
	for _, f := range infos[0].Files {
		if f.Offset < last {
			t.Errorf("%s at %d is not after %d", f.Path, f.Offset, last)
		}
		last = f.Offset + f.Size
	}
	if infos[0].Files[0].Path != "etc/hostname" {
		t.Errorf("first file is %s", infos[0].Files[0].Path)
	}
}

func TestMinimalSize(t *testing.T) {
	b := mustBuilder(t, Options{OmitLostAndFound: true})
	addFile(t, b, "file", pattern(10000, 9), 0o644)
	m, info := build(t, b)
	checkImage(t, m, info.Size)

	// superblock, descriptors, 2 bitmaps and the inode table, then the
	// root directory and 3 blocks of data. Nothing is left over.
	if info.Blocks != info.UsedBlocks {
		t.Errorf("%d blocks, %d used", info.Blocks, info.UsedBlocks)
	}
	if want := int64(1+1+2+2+1+3) * 4096; info.Size != want {
		t.Errorf("size %d, want %d", info.Size, want)
	}
	if info.Groups != 1 {
		t.Errorf("%d groups", info.Groups)
	}
}

func TestLargerImage(t *testing.T) {
	b := mustBuilder(t, Options{Size: 1 << 30, BytesPerInode: 1 << 20})
	files := populate(t, b)
	m := &memFile{}
	cw := &countingWriter{m: m}
	info, err := b.Build(cw)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size < 1<<30 || info.Size >= 1<<30+int64(4096*2) {
		t.Errorf("size %d", info.Size)
	}
	if info.Groups != 8 {
		t.Errorf("%d groups", info.Groups)
	}
	if info.Inodes < 1024 {
		t.Errorf("%d inodes", info.Inodes)
	}
	c := checkImage(t, m, info.Size)
	for name, want := range files {
		if !bytes.Equal(c.nodes[name].data, want) {
			t.Errorf("%s: wrong content", name)
		}
	}
	// Free space is not written.
	if cw.bytes > 4<<20+1<<20 {
		t.Errorf("wrote %d bytes for a %d byte image", cw.bytes, info.Size)
	}

	// The same through a file system file, which is sparse.
	path := filepath.Join(t.TempDir(), "image")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b2 := mustBuilder(t, Options{Size: 1 << 30, BytesPerInode: 1 << 20})
	populate(t, b2)
	info2, err := b2.Build(f)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.Stat()
	if st.Size() != info2.Size {
		t.Errorf("file is %d bytes, want %d", st.Size(), info2.Size)
	}
	checkImage(t, f, st.Size())
}

type countingWriter struct {
	m     *memFile
	bytes int
}

func (c *countingWriter) WriteAt(p []byte, off int64) (int, error) {
	c.bytes += len(p)
	return c.m.WriteAt(p, off)
}
func (c *countingWriter) Truncate(n int64) error { return c.m.Truncate(n) }

func TestWriterWithoutTruncate(t *testing.T) {
	b := mustBuilder(t, Options{Size: 8 << 20})
	addFile(t, b, "f", []byte("hello"), 0o644)
	m := &memFile{}
	info, err := b.Build(noTruncate{m})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(m.b)) != info.Size {
		t.Errorf("image is %d bytes, want %d", len(m.b), info.Size)
	}
	checkImage(t, m, info.Size)
}

func TestManyEntries(t *testing.T) {
	b := mustBuilder(t, Options{BlockSize: 1024})
	want := map[string][]byte{}
	for i := 0; i < 3000; i++ {
		name := "d/" + strings.Repeat("n", 20) + string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676%26)) + string(rune('0'+i%10))
		content := pattern(i%700, byte(i))
		addFile(t, b, name, content, 0o600)
		want[name] = content
	}
	addFile(t, b, "d/"+strings.Repeat("L", 255), []byte("long name"), 0o600)
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	if len(c.nodes["d"].children) != 3001 {
		t.Errorf("%d entries", len(c.nodes["d"].children))
	}
	for name, content := range want {
		if !bytes.Equal(c.nodes[name].data, content) {
			t.Fatalf("%s: wrong content", name)
		}
	}
}

func TestManyInodes(t *testing.T) {
	// 1 KiB blocks allow 8192 inodes per group.
	b := mustBuilder(t, Options{BlockSize: 1024})
	for i := 0; i < 20000; i++ {
		if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: fmt.Sprintf("d%d/l%d", i%97, i), Linkname: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	if info.Groups < 3 || info.UsedInodes < 20000 {
		t.Errorf("%d groups for %d inodes", info.Groups, info.UsedInodes)
	}
	if len(c.nodes) < 20000 {
		t.Errorf("%d nodes", len(c.nodes))
	}
}

func TestMultiExtentFile(t *testing.T) {
	// Larger than one extent (32 MiB with 1 KiB blocks), but sparse.
	dir := t.TempDir()
	src := filepath.Join(dir, "sparse")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	const size = 5*32<<20 + 777 // 6 extents: needs an extent tree block
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("tail"), size-4); err != nil {
		t.Fatal(err)
	}
	f.Close()

	b := mustBuilder(t, Options{BlockSize: 1024, OmitLostAndFound: true})
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "big", Size: size, Mode: 0o644}
	if err := b.WriteRegularFromPath(hdr, src); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "image")
	out, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	info, err := b.Build(out)
	if err != nil {
		t.Fatal(err)
	}
	c := checkImage(t, out, info.Size)
	n := c.nodes["big"]
	if len(n.extents) != 6 || !n.contiguous {
		t.Errorf("extents %v, contiguous %v", n.extents, n.contiguous)
	}
	if !bytes.HasSuffix(n.data, []byte("tail")) || len(n.data) != size {
		t.Error("wrong content")
	}
	if info.Files[0].Offset != int64(n.extents[0].phys)*1024 {
		t.Errorf("offset %d, first extent at %d", info.Files[0].Offset, n.extents[0].phys*1024)
	}
}

func TestExtentTreeDepth(t *testing.T) {
	// Drive the tree encoder directly with more extents than a depth-1 tree
	// of 1 KiB blocks can index.
	const extents = 4*84*84/2 + 5
	exts := make([]extent, extents)
	for i := range exts {
		exts[i] = extent{logical: uint32(i) * 3, length: 3, phys: 1000 + uint64(i)*3}
	}
	treeBlocks := treeBlockCount(extents, 1024)
	n := &node{planned: planned{ino: 12, treeBlocks: make([]uint64, treeBlocks)}}
	for i := range n.planned.treeBlocks {
		n.planned.treeBlocks[i] = 50 + uint64(i)
	}
	m := &memFile{}
	e := &emitter{w: m, bs: 1024, seed: 1234, zero: make([]byte, 4096)}
	iblock := make([]byte, 60)
	e.extentRoot(n, exts, iblock)

	c := &checker{t: t, r: m, bs: 1024, seed: 1234}
	var tree []uint64
	got := c.treeEntries(12, iblock, true, -1, &tree)
	if len(got) != extents {
		t.Fatalf("got %d extents, want %d", len(got), extents)
	}
	for i, x := range got {
		if x.logical != exts[i].logical || x.phys != exts[i].phys || x.length != 3 {
			t.Fatalf("extent %d: %+v", i, x)
		}
	}
	if uint64(len(tree)) != treeBlocks {
		t.Errorf("walked %d tree blocks, planned %d", len(tree), treeBlocks)
	}
	if iblock[6] != 2 {
		t.Errorf("root depth %d, want 2", iblock[6])
	}
}

func TestXattrs(t *testing.T) {
	b := mustBuilder(t, Options{})
	small := &tar.Header{Typeflag: tar.TypeReg, Name: "cap", Mode: 0o755, Size: 1, PAXRecords: map[string]string{
		"SCHILY.xattr.security.capability": "\x01\x00\x00\x02",
		"SCHILY.xattr.user.k":              "v",
	}}
	if err := b.WriteRegular(small, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	bigValue := strings.Repeat("v", 300)
	large := &tar.Header{Typeflag: tar.TypeDir, Name: "large", Mode: 0o755, PAXRecords: map[string]string{
		"SCHILY.xattr.user.big":   bigValue,
		"SCHILY.xattr.user.other": "o",
	}}
	if err := b.WriteHeader(large); err != nil {
		t.Fatal(err)
	}
	acl := []byte{2, 0, 0, 0, 1, 0, 6, 0, 0xff, 0xff, 0xff, 0xff, 2, 0, 4, 0, 5, 0, 0, 0, 0x20, 0, 4, 0, 0xff, 0xff, 0xff, 0xff}
	withACL := &tar.Header{Typeflag: tar.TypeReg, Name: "acl", Mode: 0o644, PAXRecords: map[string]string{"SCHILY.xattr.system.posix_acl_access": string(acl)}}
	if err := b.WriteHeader(withACL); err != nil {
		t.Fatal(err)
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)

	if got := c.nodes["cap"].xattrs; len(got) != 2 || got["security.capability"] != "\x01\x00\x00\x02" || got["user.k"] != "v" {
		t.Errorf("cap xattrs %q", got)
	}
	if got := c.nodes["large"].xattrs; len(got) != 2 || got["user.big"] != bigValue || got["user.other"] != "o" {
		t.Errorf("large xattrs %q", got)
	}
	wantACL := []byte{1, 0, 0, 0, 1, 0, 6, 0, 2, 0, 4, 0, 5, 0, 0, 0, 0x20, 0, 4, 0}
	if got := c.nodes["acl"].xattrs["system.posix_acl_access"]; got != string(wantACL) {
		t.Errorf("acl %v, want %v", []byte(got), wantACL)
	}

	huge := &tar.Header{Typeflag: tar.TypeDir, Name: "huge", PAXRecords: map[string]string{"SCHILY.xattr.user.x": strings.Repeat("v", 5000)}}
	b2 := mustBuilder(t, Options{})
	if err := b2.WriteHeader(huge); err != nil {
		t.Fatal(err)
	}
	if _, err := b2.Build(&memFile{}); err == nil {
		t.Error("oversized xattr accepted")
	}
}

func TestTimesAndOwners(t *testing.T) {
	b := mustBuilder(t, Options{Time: time.Unix(1_700_000_000, 0)})
	late := time.Unix(1<<32+5, 123456789)
	hdr := &tar.Header{Typeflag: tar.TypeReg, Name: "f", Mode: 0o4755, Uid: 70000, Gid: 4000000, ModTime: late, Size: 0}
	if err := b.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	early := &tar.Header{Typeflag: tar.TypeReg, Name: "old", ModTime: time.Unix(-5, 0)}
	if err := b.WriteHeader(early); err != nil {
		t.Fatal(err)
	}
	addFile(t, b, "default", []byte("d"), 0o644)
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)

	n := c.nodes["f"]
	if n.mtime != late.Unix() || n.nsec != 123456789 || n.uid != 70000 || n.gid != 4000000 || n.mode != 0x8000|0o4755 {
		t.Errorf("f: %+v", n)
	}
	if c.nodes["old"].mtime != -5 {
		t.Errorf("old: mtime %d", c.nodes["old"].mtime)
	}
	if c.nodes["default"].mtime != 1_700_000_000 {
		t.Errorf("default: mtime %d", c.nodes["default"].mtime)
	}
}

func TestReplacementAndMerge(t *testing.T) {
	b := mustBuilder(t, Options{})
	addFile(t, b, "a/file", []byte("one"), 0o644)
	addFile(t, b, "a/other", []byte("keep"), 0o644)
	addFile(t, b, "a/file", []byte("replaced"), 0o600)
	addDir(t, b, "a", 0o700) // updates the directory, keeps its children
	addFile(t, b, "gone/x", []byte("x"), 0o644)
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "gone", Linkname: "a"}); err != nil {
		t.Fatal(err)
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)

	if string(c.nodes["a/file"].data) != "replaced" || c.nodes["a/file"].mode != 0x8000|0o600 {
		t.Errorf("a/file: %q mode %#o", c.nodes["a/file"].data, c.nodes["a/file"].mode)
	}
	if string(c.nodes["a/other"].data) != "keep" || c.nodes["a"].mode != 0x4000|0o700 {
		t.Error("directory update lost children or mode")
	}
	if c.nodes["gone"].target != "a" {
		t.Error("symlink did not replace the directory")
	}
	if _, ok := c.nodes["gone/x"]; ok {
		t.Error("child of replaced directory survived")
	}
	// The replaced file's data does not take up space.
	for _, f := range info.Files {
		if f.Size == 3 || f.Path == "gone/x" {
			t.Errorf("stale file %+v", f)
		}
	}
}

func TestSpool(t *testing.T) {
	spool := t.TempDir()
	b, err := NewBuilder(Options{SpoolDir: spool, SpoolThreshold: 16})
	if err != nil {
		t.Fatal(err)
	}
	content := pattern(100000, 7)
	addFile(t, b, "big", content, 0o644)
	addFile(t, b, "small", []byte("tiny"), 0o644)
	if entries, _ := os.ReadDir(spool); len(entries) != 1 {
		t.Errorf("%d spool files", len(entries))
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	if !bytes.Equal(c.nodes["big"].data, content) || string(c.nodes["small"].data) != "tiny" {
		t.Error("wrong content")
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(spool); len(entries) != 0 {
		t.Errorf("%d spool files left", len(entries))
	}
}

func TestImportTarAndFS(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "./app/", Mode: 0o755})
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "./app/run", Mode: 0o755, Size: 4})
	tw.Write([]byte("code"))
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "app/link", Linkname: "run"})
	tw.Close()

	b := mustBuilder(t, Options{})
	if err := b.ImportTar(&buf); err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{
		"conf/a.txt":  {Data: []byte("a"), Mode: 0o640},
		"conf/b/c.sh": {Data: []byte("#!"), Mode: 0o755},
		"conf/link":   {Data: []byte("a.txt"), Mode: fsModeSymlink},
	}
	if err := b.AddFS(fsys, "opt"); err != nil {
		t.Fatal(err)
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	if string(c.nodes["app/run"].data) != "code" || c.nodes["app/run"].mode != 0x8000|0o755 {
		t.Error("tar import")
	}
	if c.nodes["app/link"].target != "run" {
		t.Error("tar symlink")
	}
	if string(c.nodes["opt/conf/b/c.sh"].data) != "#!" || c.nodes["opt/conf/a.txt"].mode != 0x8000|0o640 {
		t.Error("fs import")
	}
	if c.nodes["opt/conf/link"].target != "a.txt" {
		t.Error("fs symlink")
	}
}

func TestErrors(t *testing.T) {
	b := mustBuilder(t, Options{})
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "../escape"}); err == nil {
		t.Error("path escaping the root accepted")
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: strings.Repeat("a", 256)}); err == nil {
		t.Error("overlong name accepted")
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 3}); err == nil {
		t.Error("regular file without content accepted")
	}
	if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 10}, strings.NewReader("short")); err == nil {
		t.Error("short content accepted")
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "l", Linkname: "missing"}); err == nil {
		t.Error("dangling hard link accepted")
	}
	addFile(t, b, "file", []byte("x"), 0o644)
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "file/sub"}); err == nil {
		t.Error("directory below a regular file accepted")
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "s", Linkname: strings.Repeat("x", 5000)}); err == nil {
		t.Error("overlong symlink target accepted")
	}
	if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "d", PAXRecords: map[string]string{"SCHILY.xattr.bogus.name": "v"}}); err == nil {
		t.Error("unknown xattr namespace accepted")
	}
	for _, bad := range []int{100, 3000, 1 << 20} {
		if _, err := NewBuilder(Options{BlockSize: bad}); err == nil {
			t.Errorf("block size %d accepted", bad)
		}
	}

	// Content that shrinks between adding and building is an error.
	src := filepath.Join(t.TempDir(), "f")
	os.WriteFile(src, []byte("12345"), 0o644)
	b2 := mustBuilder(t, Options{})
	if err := b2.WriteRegularFromPath(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 5}, src); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(src, []byte("123"), 0o644)
	if _, err := b2.Build(&memFile{}); err == nil {
		t.Error("changed file accepted")
	}
	if _, err := b2.Build(&memFile{}); err == nil {
		t.Error("second build accepted")
	}
}

func TestZeroBlocksNotWritten(t *testing.T) {
	b := mustBuilder(t, Options{OmitLostAndFound: true})
	addFile(t, b, "zeros", make([]byte, 1<<20), 0o644)
	content := append(make([]byte, 8192), []byte("data")...)
	addFile(t, b, "sparse", content, 0o644)
	cw := &countingWriter{m: &memFile{}}
	info, err := b.Build(cw)
	if err != nil {
		t.Fatal(err)
	}
	if cw.bytes > 64<<10 {
		t.Errorf("wrote %d bytes for 1 MiB of zeros", cw.bytes)
	}
	c := checkImage(t, cw.m, info.Size)
	if !bytes.Equal(c.nodes["sparse"].data, content) || len(c.nodes["zeros"].data) != 1<<20 {
		t.Error("wrong content")
	}
}

func TestNeverReadsReaderTwice(t *testing.T) {
	b := mustBuilder(t, Options{})
	r := io.LimitReader(strings.NewReader(strings.Repeat("a", 100)), 100)
	if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "f", Size: 50}, r); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(r)
	if len(rest) != 50 {
		t.Errorf("builder consumed %d bytes more than the declared size", 50-len(rest))
	}
}

const fsModeSymlink = fs.ModeSymlink | 0o777

// TestDumpImages writes sample images to $EXT4_DUMP_DIR so they can be checked
// with the real e2fsprogs.
func TestDumpImages(t *testing.T) {
	dir := os.Getenv("EXT4_DUMP_DIR")
	if dir == "" {
		t.Skip("EXT4_DUMP_DIR not set")
	}
	for name, opts := range map[string]Options{
		"4k":      {},
		"1k":      {BlockSize: 1024},
		"2k":      {BlockSize: 2048},
		"large":   {Size: 1 << 30, BytesPerInode: 1 << 20},
		"nolf":    {OmitLostAndFound: true},
		"label":   {Label: "rules_img"},
		"many-1k": {BlockSize: 1024, ExtraInodes: 20000},
		"dedup":   {Deduplicate: true},
	} {
		b := mustBuilder(t, opts)
		populate(t, b)
		if opts.Deduplicate {
			dup := pattern(60000, 21)
			for _, n := range []string{"dup/one", "dup/two", "other/three"} {
				if err := b.WriteRegularDeduplicated(&tar.Header{Typeflag: tar.TypeReg, Name: n, Size: int64(len(dup)), Mode: 0o644}, bytes.NewReader(dup)); err != nil {
					t.Fatal(err)
				}
			}
		}
		f, err := os.Create(filepath.Join(dir, name+".img"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := b.Build(f); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}

func TestDigestFiles(t *testing.T) {
	b := mustBuilder(t, Options{DigestFiles: true})
	files := populate(t, b)
	_, info := build(t, b)
	for _, f := range info.Files {
		name := f.Path
		if name == "bin/hard" {
			name = "bin/odd"
		}
		if sum := sha256.Sum256(files[name]); !bytes.Equal(f.Digest, sum[:]) {
			t.Errorf("%s: digest %x, want %x", f.Path, f.Digest, sum)
		}
	}

	b2 := mustBuilder(t, Options{})
	populate(t, b2)
	_, info2 := build(t, b2)
	if info2.Files[0].Digest != nil {
		t.Error("digest computed without DigestFiles")
	}
}

func TestDeduplicate(t *testing.T) {
	content := pattern(50000, 11)
	add := func(b *Builder, name string, content []byte, mode int64, mutate func(*tar.Header)) {
		t.Helper()
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: mode, Uid: 5, ModTime: time.Unix(1_600_000_000, 0)}
		if mutate != nil {
			mutate(hdr)
		}
		if err := b.WriteRegularDeduplicated(hdr, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	populateDup := func(b *Builder) {
		add(b, "a", content, 0o644, nil)
		add(b, "dir/b", content, 0o644, nil)                                                  // identical: merged
		add(b, "c-mode", content, 0o600, nil)                                                 // other mode
		add(b, "c-owner", content, 0o644, func(h *tar.Header) { h.Uid = 6 })                  // other owner
		add(b, "c-time", content, 0o644, func(h *tar.Header) { h.ModTime = time.Unix(1, 0) }) // other time
		add(b, "c-subsecond", content, 0o644, func(h *tar.Header) { h.ModTime = h.ModTime.Add(time.Millisecond) }) // other time, below the second
		add(b, "c-xattr", content, 0o644, func(h *tar.Header) {
			h.PAXRecords = map[string]string{"SCHILY.xattr.user.k": "v"}
		})
		other := append([]byte(nil), content...)
		other[777]++
		add(b, "d-content", other, 0o644, nil) // same size, other content
		addFile(t, b, "e-plain", content, 0o644)
		if err := b.WriteRegularFromPathDeduplicated(&tar.Header{Typeflag: tar.TypeReg, Name: "f-path", Size: int64(len(content)), Mode: 0o644, Uid: 5, ModTime: time.Unix(1_600_000_000, 0)}, writeTemp(t, content)); err != nil {
			t.Fatal(err)
		}
		if err := b.WriteRegularDeduplicatedKnownHash(&tar.Header{Typeflag: tar.TypeReg, Name: "g-known", Size: int64(len(content)), Mode: 0o644, Uid: 5, ModTime: time.Unix(1_600_000_000, 0)}, bytes.NewReader(content), func() []byte { s := sha256.Sum256(content); return s[:] }()); err != nil {
			t.Fatal(err)
		}
	}

	off := mustBuilder(t, Options{})
	populateDup(off)
	_, offInfo := build(t, off)

	on := mustBuilder(t, Options{Deduplicate: true})
	populateDup(on)
	m, onInfo := build(t, on)
	c := checkImage(t, m, onInfo.Size)

	// a, dir/b, f-path and g-known are one file with four links.
	for _, name := range []string{"dir/b", "f-path", "g-known"} {
		if c.nodes[name].ino != c.nodes["a"].ino {
			t.Errorf("%s was not merged with a", name)
		}
	}
	if c.nodes["a"].links != 4 {
		t.Errorf("a has %d links, want 4", c.nodes["a"].links)
	}
	// c-subsecond differs only below the second, which the inode records, so
	// merging it would change its timestamp.
	for _, name := range []string{"c-mode", "c-owner", "c-time", "c-subsecond", "c-xattr", "d-content", "e-plain"} {
		if c.nodes[name].ino == c.nodes["a"].ino || c.nodes[name].links != 1 {
			t.Errorf("%s was merged but must not be", name)
		}
	}
	if c.nodes["c-subsecond"].nsec != uint32(time.Millisecond) {
		t.Errorf("c-subsecond lost its sub-second time: %d ns", c.nodes["c-subsecond"].nsec)
	}
	if !bytes.Equal(c.nodes["a"].data, content) || !bytes.Equal(c.nodes["g-known"].data, content) {
		t.Error("wrong content")
	}
	if onInfo.UsedInodes != offInfo.UsedInodes-3 {
		t.Errorf("%d inodes in use, want %d", onInfo.UsedInodes, offInfo.UsedInodes-3)
	}
	// Three copies of 13 blocks less.
	if want := offInfo.UsedBlocks - 3*13; onInfo.UsedBlocks != want {
		t.Errorf("%d blocks in use, want %d", onInfo.UsedBlocks, want)
	}
	if len(onInfo.Files) != len(offInfo.Files)-3 {
		t.Errorf("%d file layouts, want %d", len(onInfo.Files), len(offInfo.Files)-3)
	}

	// Without the option, nothing is merged, even through the Deduplicated methods.
	moff := &memFile{}
	{
		b := mustBuilder(t, Options{})
		populateDup(b)
		if _, err := b.Build(moff); err != nil {
			t.Fatal(err)
		}
	}
	coff := checkImage(t, moff, int64(len(moff.b)))
	if coff.nodes["a"].ino == coff.nodes["dir/b"].ino || coff.nodes["a"].links != 1 {
		t.Error("files were merged without Options.Deduplicate")
	}
}

func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDeduplicateLinkLimit(t *testing.T) {
	b := mustBuilder(t, Options{Deduplicate: true})
	const files = maxLinks + 1500
	for i := 0; i < files; i++ {
		hdr := &tar.Header{Typeflag: tar.TypeReg, Name: fmt.Sprintf("d/f%d", i), Mode: 0o644}
		if err := b.WriteRegularDeduplicated(hdr, nil); err != nil {
			t.Fatal(err)
		}
	}
	m, info := build(t, b)
	c := checkImage(t, m, info.Size)
	inodes := map[uint32]int{}
	for name, n := range c.nodes {
		if strings.HasPrefix(name, "d/f") {
			inodes[n.ino]++
		}
	}
	if len(inodes) != 2 {
		t.Fatalf("%d inodes for %d identical files, want 2 (link limit)", len(inodes), files)
	}
	for ino, n := range inodes {
		if n > maxLinks {
			t.Errorf("inode %d has %d links", ino, n)
		}
	}
}

// TestDeterminismStress builds the same tree many times with every feature that
// iterates over a map (directories, extended attributes, deduplication, hard
// links, derived UUID) and requires identical images. Go randomizes map
// iteration per run, so an unsorted iteration shows up quickly.
func TestDeterminismStress(t *testing.T) {
	build := func() []byte {
		b := mustBuilder(t, Options{Deduplicate: true, DigestFiles: true, BlockSize: 1024})
		for i := 0; i < 400; i++ {
			content := pattern(100+i%7*3000, byte(i%5)) // many duplicates
			hdr := &tar.Header{
				Typeflag: tar.TypeReg, Name: fmt.Sprintf("d%d/f%d", i%11, i), Size: int64(len(content)),
				Mode: 0o644, Uid: i % 3, ModTime: time.Unix(1_600_000_000+int64(i%4), 0),
				PAXRecords: map[string]string{
					"SCHILY.xattr.user.a": "1", "SCHILY.xattr.user.b": "2", "SCHILY.xattr.security.capability": "x",
					"SCHILY.xattr.user.c": strings.Repeat("c", i%40),
				},
			}
			if err := b.WriteRegularDeduplicated(hdr, bytes.NewReader(content)); err != nil {
				t.Fatal(err)
			}
			if i%9 == 0 {
				if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: fmt.Sprintf("links/l%d", i), Linkname: hdr.Name}); err != nil {
					t.Fatal(err)
				}
			}
		}
		for i := 0; i < 50; i++ {
			if err := b.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: fmt.Sprintf("s/%d", i), Linkname: fmt.Sprint(i)}); err != nil {
				t.Fatal(err)
			}
		}
		m, _ := build(t, b)
		return m.b
	}
	first := build()
	for i := 0; i < 20; i++ {
		if got := build(); !bytes.Equal(got, first) {
			t.Fatalf("build %d differs from the first one", i+2)
		}
	}
}
