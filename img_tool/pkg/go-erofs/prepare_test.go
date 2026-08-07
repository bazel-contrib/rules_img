package erofs_test

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"testing"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs/internal/erofstest"
)

// writeFile adds a regular file with the given content to fsys.
func writeFile(t testing.TB, fsys *erofs.Writer, name, content string) {
	t.Helper()
	f, err := fsys.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
}

// prepareAndWrite prepares fsys, serializes it with WriteTo, and checks the
// image against the planned layout.
func prepareAndWrite(t testing.TB, fsys *erofs.Writer) (*erofs.Layout, []byte) {
	t.Helper()
	layout, err := fsys.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var buf bytes.Buffer
	n, err := fsys.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n != layout.ImageSize {
		t.Errorf("WriteTo wrote %d bytes, layout planned %d", n, layout.ImageSize)
	}
	if int64(buf.Len()) != layout.ImageSize {
		t.Errorf("image is %d bytes, layout planned %d", buf.Len(), layout.ImageSize)
	}
	if layout.ImageSize%int64(layout.BlockSize) != 0 {
		t.Errorf("image size %d is not a multiple of the block size %d", layout.ImageSize, layout.BlockSize)
	}
	erofstest.FsckErofsBytes(t, buf.Bytes())
	return layout, buf.Bytes()
}

// checkExtentsTile verifies that the extents and their padding tile
// [MetaSize, ImageSize) exactly, in ascending order.
func checkExtentsTile(t testing.TB, layout *erofs.Layout) {
	t.Helper()
	pos := layout.MetaSize
	for i, e := range layout.Extents {
		if e.Offset != pos {
			t.Errorf("extent %d (%s): offset %d, want %d", i, e.Path, e.Offset, pos)
		}
		if e.Offset%int64(layout.BlockSize) != 0 {
			t.Errorf("extent %d (%s): offset %d is not block-aligned", i, e.Path, e.Offset)
		}
		if (e.Size+e.Pad)%int64(layout.BlockSize) != 0 {
			t.Errorf("extent %d (%s): size %d + pad %d is not a whole number of blocks", i, e.Path, e.Size, e.Pad)
		}
		pos = e.Offset + e.Size + e.Pad
	}
	if pos != layout.ImageSize {
		t.Errorf("extents end at %d, image size is %d", pos, layout.ImageSize)
	}
}

// extentsFor returns the extents belonging to the named path.
func extentsFor(layout *erofs.Layout, path string) []erofs.Extent {
	var out []erofs.Extent
	for _, e := range layout.Extents {
		if e.Path == path {
			out = append(out, e)
		}
	}
	return out
}

// TestPrepareWriteToAgreement checks the core contract Prepare promises: the
// image is exactly ImageSize bytes, every file-data extent holds that file's
// bytes verbatim followed by zero padding, and the extents tile the data area.
func TestPrepareWriteToAgreement(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))

	// A payload larger than one block, so it must go out of line, plus a small
	// one that is tail-packed, a multi-block directory and a long symlink.
	big := strings.Repeat("0123456789abcdef", 1024) // 16 KiB
	writeFile(t, fsys, "/big.bin", big)
	writeFile(t, fsys, "/small.txt", "small\n")
	if err := fsys.Mkdir("/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 512 {
		writeFile(t, fsys, fmt.Sprintf("/dir/f%03d", i), fmt.Sprintf("content %d\n", i))
	}
	if err := fsys.Symlink(strings.Repeat("a/", 200)+"target", "/link"); err != nil {
		t.Fatal(err)
	}

	layout, image := prepareAndWrite(t, fsys)
	checkExtentsTile(t, layout)

	bigExtents := extentsFor(layout, "/big.bin")
	if len(bigExtents) != 1 {
		t.Fatalf("/big.bin: %d extents, want 1", len(bigExtents))
	}
	e := bigExtents[0]
	if e.Kind != erofs.ExtentFileData {
		t.Errorf("/big.bin: kind %v, want %v", e.Kind, erofs.ExtentFileData)
	}
	if e.Size != int64(len(big)) {
		t.Errorf("/big.bin: extent size %d, want %d", e.Size, len(big))
	}
	if got := string(image[e.Offset : e.Offset+e.Size]); got != big {
		t.Error("/big.bin: extent bytes do not match the source content")
	}
	for i, b := range image[e.Offset+e.Size : e.Offset+e.Size+e.Pad] {
		if b != 0 {
			t.Errorf("/big.bin: padding byte %d is %#x, want 0", i, b)
			break
		}
	}

	// A consumer that has the image's metadata but not its file payloads must
	// still be able to walk the whole tree: only ExtentFileData ranges carry
	// file content, so zeroing exactly those must leave the directory
	// structure (including out-of-line dirent blocks and symlink targets)
	// intact. This is what reading an EROFS layer out of a compact stream
	// without fetching any blob does.
	sparse := make([]byte, len(image))
	copy(sparse, image)
	for _, e := range layout.Extents {
		if e.Kind != erofs.ExtentFileData {
			continue
		}
		for i := e.Offset; i < e.Offset+e.Size; i++ {
			sparse[i] = 0
		}
	}
	efs, err := erofs.Open(bytes.NewReader(sparse))
	if err != nil {
		t.Fatalf("Open payload-free image: %v", err)
	}
	var files int
	if err := fs.WalkDir(efs, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files++
		}
		return nil
	}); err != nil {
		t.Fatalf("walking payload-free image: %v", err)
	}
	if want := 512 + 2; files != want {
		t.Errorf("walked %d regular files, want %d", files, want)
	}
	erofstest.CheckSymlink(t, efs, "link", strings.Repeat("a/", 200)+"target")

	// And the full image reads back correctly.
	efs, err = erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	erofstest.CheckFile(t, efs, "big.bin", big)
	erofstest.CheckFile(t, efs, "small.txt", "small\n")
	erofstest.CheckFile(t, efs, "dir/f511", "content 511\n")
	erofstest.CheckSymlink(t, efs, "link", strings.Repeat("a/", 200)+"target")
}

// TestPrepareIdempotent checks that Prepare can be called repeatedly and that
// the tree is frozen afterwards.
func TestPrepareIdempotent(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	writeFile(t, fsys, "/a", "a\n")

	first, err := fsys.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	second, err := fsys.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("Prepare returned a different layout on the second call")
	}
	if _, err := fsys.Create("/b"); err == nil {
		t.Error("Create after Prepare succeeded, want an error")
	}
	if err := fsys.Mkdir("/c", 0o755); err == nil {
		t.Error("Mkdir after Prepare succeeded, want an error")
	}
}

// TestWriteToMatchesClose checks that the metadata-first image produced by
// WriteTo describes the same filesystem as the data-first image Close produces.
func TestWriteToMatchesClose(t *testing.T) {
	build := func(fsys *erofs.Writer) {
		writeFile(t, fsys, "/big.bin", strings.Repeat("x", 9000))
		writeFile(t, fsys, "/small", "s\n")
		if err := fsys.Mkdir("/d", 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, fsys, "/d/inner", "inner\n")
		if err := fsys.Symlink("/d/inner", "/l"); err != nil {
			t.Fatal(err)
		}
		if err := fsys.Setxattr("/d", "user.k", "v"); err != nil {
			t.Fatal(err)
		}
	}

	var seekBuf testBuffer
	seekable := erofs.Create(&seekBuf, erofs.WithBuildTime(0, 0))
	build(seekable)
	if err := seekable.Close(); err != nil {
		t.Fatal("Close:", err)
	}

	streamed := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	build(streamed)
	_, streamImage := prepareAndWrite(t, streamed)

	if len(seekBuf.Bytes()) != len(streamImage) {
		t.Errorf("image sizes differ: seekable %d, streamed %d", len(seekBuf.Bytes()), len(streamImage))
	}

	for name, image := range map[string][]byte{"seekable": seekBuf.Bytes(), "streamed": streamImage} {
		efs, err := erofs.Open(bytes.NewReader(image))
		if err != nil {
			t.Fatalf("%s: Open: %v", name, err)
		}
		erofstest.CheckFile(t, efs, "big.bin", strings.Repeat("x", 9000))
		erofstest.CheckFile(t, efs, "small", "s\n")
		erofstest.CheckFile(t, efs, "d/inner", "inner\n")
		erofstest.CheckSymlink(t, efs, "l", "/d/inner")
		erofstest.CheckXattrs(t, efs, "d", map[string]string{"user.k": "v"})
		if st := erofstest.Stat(t, efs, "d"); st.Mode.Perm() != 0o700 {
			t.Errorf("%s: /d mode %v, want 0700", name, st.Mode.Perm())
		}
	}
}

// TestLink checks hardlinks: one inode, two dirents, nlink counted.
func TestLink(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	content := strings.Repeat("linked payload\n", 1000)
	writeFile(t, fsys, "/orig", content)
	if err := fsys.Link("/orig", "/dir/alias"); err != nil {
		t.Fatal("Link:", err)
	}
	if err := fsys.Link("/dir/alias", "/alias2"); err != nil {
		t.Fatal("Link through a hardlink:", err)
	}

	layout, image := prepareAndWrite(t, fsys)
	checkExtentsTile(t, layout)

	// The payload is stored exactly once.
	if got := len(extentsFor(layout, "/orig")); got != 1 {
		t.Errorf("/orig: %d extents, want 1", got)
	}
	for _, p := range []string{"/dir/alias", "/alias2"} {
		if got := len(extentsFor(layout, p)); got != 0 {
			t.Errorf("%s: %d extents, want 0 (shares the target's inode)", p, got)
		}
	}

	efs, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	erofstest.CheckFile(t, efs, "orig", content)
	erofstest.CheckFile(t, efs, "dir/alias", content)
	erofstest.CheckFile(t, efs, "alias2", content)

	orig := erofstest.Stat(t, efs, "orig")
	alias := erofstest.Stat(t, efs, "dir/alias")
	if orig.Ino != alias.Ino {
		t.Errorf("hardlink resolves to inode %d, target is %d", alias.Ino, orig.Ino)
	}
	if orig.Nlink != 3 {
		t.Errorf("/orig nlink %d, want 3", orig.Nlink)
	}
}

// TestLinkRejectsDirectory checks that directories cannot be hardlinked.
func TestLinkRejectsDirectory(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	if err := fsys.Mkdir("/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Link("/d", "/e"); err == nil {
		t.Error("Link to a directory succeeded, want an error")
	}
}

// TestShareData checks that two inodes can reference one payload while keeping
// independent metadata.
func TestShareData(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	content := strings.Repeat("shared\n", 100)
	writeFile(t, fsys, "/a", content)
	writeFile(t, fsys, "/b", content)
	if err := fsys.ShareData("/a", "/b"); err != nil {
		t.Fatal("ShareData:", err)
	}
	if err := fsys.Chmod("/b", 0o600); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chown("/b", 1000, 1000); err != nil {
		t.Fatal(err)
	}

	layout, image := prepareAndWrite(t, fsys)
	checkExtentsTile(t, layout)

	if got := len(extentsFor(layout, "/a")); got != 1 {
		t.Errorf("/a: %d extents, want 1", got)
	}
	if got := len(extentsFor(layout, "/b")); got != 0 {
		t.Errorf("/b: %d extents, want 0 (shares /a's extent)", got)
	}

	efs, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	erofstest.CheckFile(t, efs, "a", content)
	erofstest.CheckFile(t, efs, "b", content)

	a, b := erofstest.Stat(t, efs, "a"), erofstest.Stat(t, efs, "b")
	if a.Ino == b.Ino {
		t.Error("shared payload collapsed the two inodes; want independent inodes")
	}
	if b.Mode.Perm() != 0o600 {
		t.Errorf("/b mode %v, want 0600", b.Mode.Perm())
	}
	if b.UID != 1000 || b.GID != 1000 {
		t.Errorf("/b owner %d:%d, want 1000:1000", b.UID, b.GID)
	}
	if a.Nlink != 1 || b.Nlink != 1 {
		t.Errorf("nlink %d/%d, want 1/1 (sharing data is not linking)", a.Nlink, b.Nlink)
	}
}

// TestShareDataKeepsSourceOutOfLine checks that a tail-packable payload is
// forced out of line once another inode references it, since an inline payload
// has no extent to share.
func TestShareDataKeepsSourceOutOfLine(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	writeFile(t, fsys, "/a", "tiny\n")
	writeFile(t, fsys, "/b", "tiny\n")
	if err := fsys.ShareData("/a", "/b"); err != nil {
		t.Fatal(err)
	}

	layout, image := prepareAndWrite(t, fsys)
	if got := len(extentsFor(layout, "/a")); got != 1 {
		t.Fatalf("/a: %d extents, want 1 (a shared payload must not be inlined)", got)
	}
	efs, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	erofstest.CheckFile(t, efs, "a", "tiny\n")
	erofstest.CheckFile(t, efs, "b", "tiny\n")
}

// TestSetToken checks that a caller value attached to an entry is reported on
// its extent.
func TestSetToken(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	writeFile(t, fsys, "/big", strings.Repeat("y", 5000))
	if err := fsys.SetToken("/big", "digest-of-big"); err != nil {
		t.Fatal(err)
	}

	layout, _ := prepareAndWrite(t, fsys)
	extents := extentsFor(layout, "/big")
	if len(extents) != 1 {
		t.Fatalf("/big: %d extents, want 1", len(extents))
	}
	if extents[0].Token != "digest-of-big" {
		t.Errorf("token %v, want %q", extents[0].Token, "digest-of-big")
	}
}

// TestWithInlineThreshold checks that the threshold decides which payloads get
// an extent of their own.
func TestWithInlineThreshold(t *testing.T) {
	for _, tc := range []struct {
		name       string
		threshold  int
		wantInline bool
	}{
		{name: "default inlines", threshold: 0, wantInline: true},
		{name: "above threshold goes out of line", threshold: 4, wantInline: false},
		{name: "below threshold inlines", threshold: 64, wantInline: true},
		{name: "negative disables inlining", threshold: -1, wantInline: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0), erofs.WithInlineThreshold(tc.threshold))
			writeFile(t, fsys, "/f", "0123456789")

			layout, image := prepareAndWrite(t, fsys)
			checkExtentsTile(t, layout)

			inline := len(extentsFor(layout, "/f")) == 0
			if inline != tc.wantInline {
				t.Errorf("inlined = %v, want %v", inline, tc.wantInline)
			}
			efs, err := erofs.Open(bytes.NewReader(image))
			if err != nil {
				t.Fatal(err)
			}
			erofstest.CheckFile(t, efs, "f", "0123456789")
		})
	}
}

// TestWithUUID checks that the superblock carries the requested UUID.
func TestWithUUID(t *testing.T) {
	uuid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0), erofs.WithUUID(uuid))
	writeFile(t, fsys, "/f", "f\n")
	_, image := prepareAndWrite(t, fsys)

	// UUID lives at offset 48 of the superblock, which starts at byte 1024.
	const uuidOffset = 1024 + 48
	if got := image[uuidOffset : uuidOffset+16]; !bytes.Equal(got, uuid[:]) {
		t.Errorf("superblock UUID %x, want %x", got, uuid)
	}
}

// TestSyntheticDirs checks the synthesized-parent hook and the report of
// directories that were never described explicitly.
func TestSyntheticDirs(t *testing.T) {
	var asked []string
	fsys := erofs.NewWriter(
		erofs.WithBuildTime(0, 0),
		erofs.WithSyntheticDirMetadata(func(p string, ancestor erofs.SyntheticDirMetadata) (erofs.SyntheticDirMetadata, bool) {
			asked = append(asked, p)
			if p == "/described" {
				// Never consulted for a directory added explicitly first, but
				// harmless if it is.
				return erofs.SyntheticDirMetadata{}, false
			}
			if p == "/opt" {
				return erofs.SyntheticDirMetadata{
					Mode:   0o700,
					UID:    70,
					GID:    70,
					Xattrs: map[string]string{"user.from": "hook"},
				}, true
			}
			// Everything else inherits the nearest ancestor's ownership, the
			// way mkfs.erofs does.
			ancestor.Mode = 0o755
			return ancestor, true
		}),
	)

	if err := fsys.Mkdir("/described", 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, fsys, "/described/f", "f\n")
	writeFile(t, fsys, "/opt/app/bin/tool", "tool\n")

	// A directory synthesized first and described afterwards keeps the
	// explicit attributes and drops out of SyntheticDirs.
	writeFile(t, fsys, "/late/f", "f\n")
	if err := fsys.Mkdir("/late", 0o711); err != nil {
		t.Fatal(err)
	}

	if want := []string{"/opt", "/opt/app", "/opt/app/bin", "/late"}; !equalStrings(asked, want) {
		t.Errorf("hook consulted for %v, want %v", asked, want)
	}
	if want := []string{"/opt", "/opt/app", "/opt/app/bin"}; !equalStrings(fsys.SyntheticDirs(), want) {
		t.Errorf("SyntheticDirs() = %v, want %v", fsys.SyntheticDirs(), want)
	}

	_, image := prepareAndWrite(t, fsys)
	efs, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	opt := erofstest.Stat(t, efs, "opt")
	if opt.Mode.Perm() != 0o700 || opt.UID != 70 || opt.GID != 70 {
		t.Errorf("/opt = %v %d:%d, want 0700 70:70", opt.Mode.Perm(), opt.UID, opt.GID)
	}
	erofstest.CheckXattrs(t, efs, "opt", map[string]string{"user.from": "hook"})
	// /opt/app inherits /opt's ownership through the hook's ancestor argument.
	app := erofstest.Stat(t, efs, "opt/app")
	if app.Mode.Perm() != 0o755 || app.UID != 70 || app.GID != 70 {
		t.Errorf("/opt/app = %v %d:%d, want 0755 70:70", app.Mode.Perm(), app.UID, app.GID)
	}
	if late := erofstest.Stat(t, efs, "late"); late.Mode.Perm() != 0o711 {
		t.Errorf("/late mode %v, want 0711 (explicit Mkdir wins)", late.Mode.Perm())
	}
}

// TestSyntheticDirsDefault checks the unchanged default: 0755, root-owned.
func TestSyntheticDirsDefault(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	writeFile(t, fsys, "/a/b/c", "c\n")

	_, image := prepareAndWrite(t, fsys)
	efs, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a", "a/b"} {
		st := erofstest.Stat(t, efs, p)
		if st.Mode.Perm() != 0o755 || st.UID != 0 || st.GID != 0 {
			t.Errorf("/%s = %v %d:%d, want 0755 0:0", p, st.Mode.Perm(), st.UID, st.GID)
		}
	}
}

// TestRemove checks last-writer-wins replacement and the guards that keep a
// hardlink or shared payload from dangling.
func TestRemove(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	writeFile(t, fsys, "/f", "first\n")

	if err := fsys.Remove("/missing"); err == nil {
		t.Error("Remove of a missing path succeeded, want an error")
	}
	if err := fsys.Remove("/"); err == nil {
		t.Error("Remove of the root succeeded, want an error")
	}

	if err := fsys.Remove("/f"); err != nil {
		t.Fatal("Remove:", err)
	}
	writeFile(t, fsys, "/f", "second\n")

	// A referenced entry cannot be removed.
	writeFile(t, fsys, "/target", "target\n")
	if err := fsys.Link("/target", "/link"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Remove("/target"); err == nil {
		t.Error("Remove of a hardlink target succeeded, want an error")
	}
	if err := fsys.Remove("/link"); err != nil {
		t.Fatal("Remove of a hardlink:", err)
	}
	if err := fsys.Remove("/target"); err != nil {
		t.Fatal("Remove after dropping the last hardlink:", err)
	}

	writeFile(t, fsys, "/src", "shared\n")
	writeFile(t, fsys, "/dst", "shared\n")
	if err := fsys.ShareData("/src", "/dst"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Remove("/src"); err == nil {
		t.Error("Remove of a shared payload source succeeded, want an error")
	}

	// A directory takes its whole subtree with it.
	writeFile(t, fsys, "/d/sub/x", "x\n")
	if err := fsys.Remove("/d"); err != nil {
		t.Fatal("Remove of a directory:", err)
	}

	_, image := prepareAndWrite(t, fsys)
	efs, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	erofstest.CheckFile(t, efs, "f", "second\n")
	if _, err := fs.Stat(efs, "d"); err == nil {
		t.Error("/d survived Remove")
	}
	if _, err := fs.Stat(efs, "link"); err == nil {
		t.Error("/link survived Remove")
	}
}

// TestWriteToDeterministic checks that two identical builds produce identical
// bytes.
func TestWriteToDeterministic(t *testing.T) {
	build := func() []byte {
		fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
		writeFile(t, fsys, "/b", strings.Repeat("b", 7000))
		writeFile(t, fsys, "/a", "a\n")
		if err := fsys.Mkdir("/d", 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, fsys, "/d/z", "z\n")
		if err := fsys.Setxattr("/d/z", "user.b", "2"); err != nil {
			t.Fatal(err)
		}
		if err := fsys.Setxattr("/d/z", "user.a", "1"); err != nil {
			t.Fatal(err)
		}
		if err := fsys.Link("/a", "/d/a"); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if _, err := fsys.WriteTo(&buf); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	if first, second := build(), build(); !bytes.Equal(first, second) {
		t.Error("two identical builds produced different bytes")
	}
}

// TestCloseAfterPrepare checks that the two finalizers are mutually exclusive.
func TestCloseAfterPrepare(t *testing.T) {
	var buf testBuffer
	fsys := erofs.Create(&buf, erofs.WithBuildTime(0, 0))
	writeFile(t, fsys, "/f", "f\n")
	if _, err := fsys.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Close(); err == nil {
		t.Error("Close after Prepare succeeded, want an error")
	}
}

// TestWriteToWithoutOutput checks that a NewWriter cannot be finalized with
// Close.
func TestWriteToWithoutOutput(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	writeFile(t, fsys, "/f", "f\n")
	if err := fsys.Close(); err == nil {
		t.Error("Close on a Writer with no output succeeded, want an error")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAddFile checks that content handed over as a reader is read once, at
// serialization time, and closed afterwards.
func TestAddFile(t *testing.T) {
	big := strings.Repeat("payload\n", 2000)
	src := &countingReader{Reader: strings.NewReader(big)}
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	if err := fsys.AddFile("/deep/dir/file", int64(len(big)), src); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chmod("/deep/dir/file", 0o600); err != nil {
		t.Fatal(err)
	}
	if src.reads != 0 {
		t.Errorf("reader was read %d times before serialization, want 0", src.reads)
	}

	layout, image := prepareAndWrite(t, fsys)
	checkExtentsTile(t, layout)
	if !src.closed {
		t.Error("reader was not closed after serialization")
	}

	efs, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	erofstest.CheckFile(t, efs, "deep/dir/file", big)
	if st := erofstest.Stat(t, efs, "deep/dir/file"); st.Mode.Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", st.Mode.Perm())
	}
}

// TestAddFileSizeMismatch checks that a lying size is reported rather than
// silently truncating the image.
func TestAddFileSizeMismatch(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	if err := fsys.AddFile("/f", 9000, strings.NewReader("short")); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.WriteTo(&bytes.Buffer{}); err == nil {
		t.Error("WriteTo succeeded with a short reader, want an error")
	}
}

// TestMknod checks that the exported mode constants drive Mknod, including the
// overlayfs whiteout shape (a character device with rdev 0).
func TestMknod(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	if err := fsys.Mknod("/whiteout", erofs.ModeChardev|0o000, 0); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mknod("/fifo", erofs.ModeFifo|0o644, 0); err != nil {
		t.Fatal(err)
	}
	_, image := prepareAndWrite(t, fsys)
	efs, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	erofstest.CheckDevice(t, efs, "whiteout", fs.ModeDevice|fs.ModeCharDevice, 0)
	erofstest.CheckDevice(t, efs, "fifo", fs.ModeNamedPipe, 0)
}

// countingReader records how often it was read and whether it was closed.
type countingReader struct {
	io.Reader
	reads  int
	closed bool
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	return r.Reader.Read(p)
}

func (r *countingReader) Close() error {
	r.closed = true
	return nil
}

// TestMkdirSynthesized checks that a directory the caller needs but does not
// describe gets the hook's attributes and is reported as synthesized, while an
// explicit description afterwards takes over.
func TestMkdirSynthesized(t *testing.T) {
	fsys := erofs.NewWriter(
		erofs.WithBuildTime(0, 0),
		erofs.WithSyntheticDirMetadata(func(_ string, ancestor erofs.SyntheticDirMetadata) (erofs.SyntheticDirMetadata, bool) {
			ancestor.Mode = 0o755
			return ancestor, true
		}),
	)
	if err := fsys.Mkdir("/described", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Chown("/described", 42, 42); err != nil {
		t.Fatal(err)
	}
	// Needed to hang something on, with no idea what it should look like.
	if err := fsys.MkdirSynthesized("/described/inner"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Setxattr("/described/inner", "user.marker", "y"); err != nil {
		t.Fatal(err)
	}
	// An existing entry is left alone.
	if err := fsys.MkdirSynthesized("/described"); err != nil {
		t.Fatal(err)
	}

	if want := []string{"/described/inner"}; !equalStrings(fsys.SyntheticDirs(), want) {
		t.Errorf("SyntheticDirs() = %v, want %v", fsys.SyntheticDirs(), want)
	}

	_, image := prepareAndWrite(t, fsys)
	efs, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	inner := erofstest.Stat(t, efs, "described/inner")
	if inner.UID != 42 || inner.GID != 42 || inner.Mode.Perm() != 0o755 {
		t.Errorf("/described/inner = %v %d:%d, want 0755 42:42 (inherited)", inner.Mode.Perm(), inner.UID, inner.GID)
	}
	erofstest.CheckXattrs(t, efs, "described/inner", map[string]string{"user.marker": "y"})
	if described := erofstest.Stat(t, efs, "described"); described.Mode.Perm() != 0o700 {
		t.Errorf("/described mode %v, want 0700", described.Mode.Perm())
	}
}

// TestMkdirSynthesizedThenDescribed checks that describing a directory after it
// was synthesized takes it out of the report.
func TestMkdirSynthesizedThenDescribed(t *testing.T) {
	fsys := erofs.NewWriter(erofs.WithBuildTime(0, 0))
	if err := fsys.MkdirSynthesized("/d"); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Mkdir("/d", 0o711); err != nil {
		t.Fatal(err)
	}
	if got := fsys.SyntheticDirs(); len(got) != 0 {
		t.Errorf("SyntheticDirs() = %v, want none", got)
	}
	_, image := prepareAndWrite(t, fsys)
	efs, err := erofs.Open(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	if st := erofstest.Stat(t, efs, "d"); st.Mode.Perm() != 0o711 {
		t.Errorf("/d mode %v, want 0711", st.Mode.Perm())
	}
}
