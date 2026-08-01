package erofscas

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	erofs "github.com/bazel-contrib/rules_img/img_tool/pkg/go-erofs"
	"github.com/klauspost/compress/zstd"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
)

// memBlobStore serves the layer's input files back by content digest, the way a
// cas-dir does when a compact stream is reconstructed.
type memBlobStore map[string][]byte

func (m memBlobStore) put(data []byte) []byte {
	digest := sha256.Sum256(data)
	m[string(digest[:])] = append([]byte(nil), data...)
	return digest[:]
}

func (m memBlobStore) ReaderForBlob(_ context.Context, digest []byte, _ int64) (io.ReadCloser, error) {
	data, ok := m[string(digest)]
	if !ok {
		return nil, fmt.Errorf("blob not found: %x", digest)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// layer is the result of recording entries into an EROFS layer.
type layer struct {
	blob        []byte // the layer blob as pushed (possibly compressed)
	image       []byte // the uncompressed EROFS image, i.e. the diff
	compactStrm []byte
	state       api.AppenderState
	store       memBlobStore
}

// buildLayer records the given entries into an EROFS layer, producing the blob,
// its compact stream, and a blob store holding every file's content.
func buildLayer(t *testing.T, compression string, record func(*testing.T, *CAS[SHA256Helper], memBlobStore), opts ...Option) layer {
	t.Helper()

	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", compression, &blob)
	if err != nil {
		t.Fatal(err)
	}

	var cs bytes.Buffer
	origComp := compactstream.OriginalCompressionNone
	if compression == "zstd" {
		origComp = compactstream.OriginalCompressionZstd
	}
	csWriter := compactstream.NewWriter(&cs, compactstream.HashAlgoSHA256, sha256.Size,
		compactstream.StreamCompressionZstd,
		compactstream.OriginalCompressionInfo{Compression: origComp, CompressionLevel: -1, CompressorJobs: 1},
		0)

	opts = append([]Option{WithCompactStreamWriter{Writer: csWriter}}, opts...)
	c := NewSHA256CASWithDigestFS(appender, digestfs.New(SHA256Helper{}), opts...)

	store := memBlobStore{}
	record(t, c, store)

	if err := c.Close(); err != nil {
		t.Fatalf("closing layer: %v", err)
	}
	state, err := appender.Finalize()
	if err != nil {
		t.Fatalf("finalizing compressor: %v", err)
	}
	if err := csWriter.SetCompressedStreamInfo(state.OuterHash, uint64(state.CompressedSize)); err != nil {
		t.Fatal(err)
	}
	if err := csWriter.Close(); err != nil {
		t.Fatalf("writing compact stream: %v", err)
	}

	image := blob.Bytes()
	if compression == "zstd" {
		dec, err := zstd.NewReader(bytes.NewReader(image))
		if err != nil {
			t.Fatal(err)
		}
		defer dec.Close()
		image, err = io.ReadAll(dec)
		if err != nil {
			t.Fatalf("decompressing layer blob: %v", err)
		}
	}
	return layer{blob: blob.Bytes(), image: image, compactStrm: cs.Bytes(), state: state, store: store}
}

// checkReconstructs rebuilds the layer blob from its compact stream and the blob
// store, and requires the result to be byte-identical. Reconstruct also verifies
// the recorded compressed-stream digest itself.
func checkReconstructs(t *testing.T, l layer) {
	t.Helper()
	var out bytes.Buffer
	if err := compactstream.Reconstruct(context.Background(), bytes.NewReader(l.compactStrm), l.store, &out); err != nil {
		t.Fatalf("reconstructing from compact stream: %v", err)
	}
	if !bytes.Equal(out.Bytes(), l.blob) {
		t.Errorf("reconstructed blob differs from the original (%d vs %d bytes)", out.Len(), len(l.blob))
	}
}

// open reads the layer's EROFS image.
func (l layer) open(t *testing.T) fs.FS {
	t.Helper()
	efs, err := erofs.Open(bytes.NewReader(l.image))
	if err != nil {
		t.Fatalf("opening EROFS image: %v", err)
	}
	return efs
}

func (l layer) stat(t *testing.T, name string) *erofs.Stat {
	t.Helper()
	fi, err := fs.Stat(l.open(t), name)
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}
	st, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		t.Fatalf("%s: Sys() is %T, want *erofs.Stat", name, fi.Sys())
	}
	return st
}

func (l layer) read(t *testing.T, name string) string {
	t.Helper()
	data, err := fs.ReadFile(l.open(t), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// writeTempFile writes content to a file in the test's temp dir and returns its
// path, standing in for a Bazel input file.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func regularHdr(name string, size int64) *tar.Header {
	return &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o755}
}

// TestLayerRoundTrip records a layer that exercises every entry type and checks
// that the image reads back correctly and that its compact stream reconstructs
// the blob bit for bit.
func TestLayerRoundTrip(t *testing.T) {
	for _, compression := range []string{"none", "zstd"} {
		t.Run(compression, func(t *testing.T) {
			// Larger than a block, so it gets an extent of its own and becomes a
			// CAS reference rather than inline bytes.
			big := strings.Repeat("payload!", 2048)
			bigPath := writeTempFile(t, "big.bin", big)

			l := buildLayer(t, compression, func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
				store.put([]byte(big))
				if err := c.WriteRegularFromPath(regularHdr("usr/bin/tool", int64(len(big))), bigPath); err != nil {
					t.Fatal(err)
				}
				if err := c.WriteRegular(regularHdr("etc/small.conf", 6), strings.NewReader("small\n")); err != nil {
					t.Fatal(err)
				}
				if err := c.WriteRegular(regularHdr("etc/empty", 0), strings.NewReader("")); err != nil {
					t.Fatal(err)
				}
				if err := c.WriteHeader(&tar.Header{
					Typeflag: tar.TypeDir, Name: "var/lib/", Mode: 0o1777,
					Uid: 100, Gid: 200,
				}); err != nil {
					t.Fatal(err)
				}
				if err := c.WriteHeader(&tar.Header{
					Typeflag: tar.TypeSymlink, Name: "usr/bin/link", Linkname: "tool",
				}); err != nil {
					t.Fatal(err)
				}
				if err := c.WriteHeader(&tar.Header{
					Typeflag: tar.TypeChar, Name: "dev/null", Mode: 0o666,
					Devmajor: 1, Devminor: 3,
				}); err != nil {
					t.Fatal(err)
				}
				if err := c.WriteHeader(&tar.Header{
					Typeflag: tar.TypeFifo, Name: "run/pipe", Mode: 0o644,
				}); err != nil {
					t.Fatal(err)
				}
				if err := c.WriteRegular(&tar.Header{
					Typeflag: tar.TypeReg, Name: "etc/shadow", Size: 3, Mode: 0o640,
					Uid: 0, Gid: 42,
					PAXRecords: map[string]string{"SCHILY.xattr.user.marker": "yes"},
				}, strings.NewReader("x\n\n")); err != nil {
					t.Fatal(err)
				}
			})

			checkReconstructs(t, l)

			if got := l.read(t, "usr/bin/tool"); got != big {
				t.Error("usr/bin/tool content does not round-trip")
			}
			if got := l.read(t, "etc/small.conf"); got != "small\n" {
				t.Errorf("etc/small.conf = %q", got)
			}
			if got := l.read(t, "etc/empty"); got != "" {
				t.Errorf("etc/empty = %q, want empty", got)
			}

			varLib := l.stat(t, "var/lib")
			if varLib.Mode.Perm() != 0o777 || varLib.Mode&fs.ModeSticky == 0 {
				t.Errorf("var/lib mode %v, want sticky 0777", varLib.Mode)
			}
			if varLib.UID != 100 || varLib.GID != 200 {
				t.Errorf("var/lib owner %d:%d, want 100:200", varLib.UID, varLib.GID)
			}

			link, err := l.open(t).(interface {
				ReadLink(string) (string, error)
			}).ReadLink("usr/bin/link")
			if err != nil {
				t.Fatal(err)
			}
			if link != "tool" {
				t.Errorf("usr/bin/link -> %q, want %q", link, "tool")
			}

			dev := l.stat(t, "dev/null")
			if dev.Mode&fs.ModeCharDevice == 0 {
				t.Errorf("dev/null mode %v, want a character device", dev.Mode)
			}
			if want := encodeDev(1, 3); dev.Rdev != want {
				t.Errorf("dev/null rdev %d, want %d", dev.Rdev, want)
			}
			if pipe := l.stat(t, "run/pipe"); pipe.Mode&fs.ModeNamedPipe == 0 {
				t.Errorf("run/pipe mode %v, want a FIFO", pipe.Mode)
			}

			shadow := l.stat(t, "etc/shadow")
			if shadow.Mode.Perm() != 0o640 || shadow.GID != 42 {
				t.Errorf("etc/shadow = %v gid %d, want 0640 gid 42", shadow.Mode.Perm(), shadow.GID)
			}
			if shadow.Xattrs["user.marker"] != "yes" {
				t.Errorf("etc/shadow xattrs %v, want user.marker=yes", shadow.Xattrs)
			}

			// The diff ID is the digest of the uncompressed image, which is
			// what makes compress.Appender usable unchanged.
			imageDigest := sha256.Sum256(l.image)
			if !bytes.Equal(l.state.ContentHash, imageDigest[:]) {
				t.Error("ContentHash is not the digest of the EROFS image")
			}
		})
	}
}

// TestSyntheticDirectoriesGetParentAttributes checks that an intermediate
// directory nobody describes inherits its ancestor's ownership, the way
// mkfs.erofs does.
func TestSyntheticDirectoriesGetParentAttributes(t *testing.T) {
	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if err := c.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: "opt/", Mode: 0o770, Uid: 70, Gid: 70,
		}); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteRegular(regularHdr("opt/app/bin/tool", 5), strings.NewReader("tool\n")); err != nil {
			t.Fatal(err)
		}
	})

	for _, p := range []string{"opt/app", "opt/app/bin"} {
		st := l.stat(t, p)
		if st.UID != 70 || st.GID != 70 {
			t.Errorf("%s owner %d:%d, want 70:70 (inherited)", p, st.UID, st.GID)
		}
		if st.Mode.Perm() != 0o775 {
			// 0755 plus the ancestor's group-writable bit, matching mkfs.erofs.
			t.Errorf("%s mode %v, want 0775", p, st.Mode.Perm())
		}
	}
}

// TestSyntheticDirectoriesDefault checks that the default policy ignores the
// surroundings: an invented directory is 0755 root:root even when its described
// ancestor is neither. This shadows a lower layer, which is documented.
func TestSyntheticDirectoriesDefault(t *testing.T) {
	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if err := c.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: "opt/", Mode: 0o770, Uid: 70, Gid: 70,
		}); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteRegular(regularHdr("opt/app/bin/tool", 5), strings.NewReader("tool\n")); err != nil {
			t.Fatal(err)
		}
	}, WithSyntheticDirectories(SyntheticDirectoriesDefault))

	for _, p := range []string{"opt/app", "opt/app/bin"} {
		st := l.stat(t, p)
		if st.UID != 0 || st.GID != 0 {
			t.Errorf("%s owner %d:%d, want 0:0", p, st.UID, st.GID)
		}
		if st.Mode.Perm() != 0o755 {
			t.Errorf("%s mode %v, want 0755", p, st.Mode.Perm())
		}
	}

	// The described ancestor keeps its own attributes under every policy.
	if st := l.stat(t, "opt"); st.UID != 70 || st.Mode.Perm() != 0o770 {
		t.Errorf("described /opt = %d:%d %v, want 70:70 0770", st.UID, st.GID, st.Mode.Perm())
	}
}

// TestSyntheticDirectoriesStrict checks that the strict policy refuses to guess.
func TestSyntheticDirectoriesStrict(t *testing.T) {
	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", "none", &blob)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSHA256CASWithDigestFS(appender, digestfs.New(SHA256Helper{}),
		WithSyntheticDirectories(SyntheticDirectoriesStrict))
	if err := c.WriteRegular(regularHdr("opt/app/tool", 5), strings.NewReader("tool\n")); err != nil {
		t.Fatal(err)
	}
	if got := c.SyntheticDirs(); len(got) != 2 {
		t.Errorf("SyntheticDirs() = %v, want /opt and /opt/app", got)
	}
	err = c.Close()
	if err == nil {
		t.Fatal("Close succeeded, want an error naming the synthesized directories")
	}
	if !strings.Contains(err.Error(), "/opt/app") {
		t.Errorf("error does not name the offending directory: %v", err)
	}
}

// TestSyntheticDirectoriesDescribedLater checks that describing a directory
// after something beneath it still counts as describing it.
func TestSyntheticDirectoriesDescribedLater(t *testing.T) {
	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if err := c.WriteRegular(regularHdr("opt/tool", 5), strings.NewReader("tool\n")); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: "opt/", Mode: 0o700, Uid: 7, Gid: 7,
		}); err != nil {
			t.Fatal(err)
		}
	})
	st := l.stat(t, "opt")
	if st.Mode.Perm() != 0o700 || st.UID != 7 {
		t.Errorf("/opt = %v uid %d, want 0700 uid 7", st.Mode.Perm(), st.UID)
	}
}

// TestDeduplicationLinks checks that repeated content is stored once and reached
// through a hardlink, matching what a tar layer records, so the two formats
// report the same link counts.
func TestDeduplicationLinks(t *testing.T) {
	content := strings.Repeat("duplicate\n", 1000)
	pathA := writeTempFile(t, "a.bin", content)

	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		store.put([]byte(content))
		for _, name := range []string{"one/tool", "two/tool", "three/tool"} {
			if err := c.WriteRegularFromPathDeduplicated(regularHdr(name, int64(len(content))), pathA); err != nil {
				t.Fatal(err)
			}
		}
		// An entry with metadata of its own cannot share the blob entry, so it
		// is stored separately and deduplicated against its own node hash.
		withMeta := func(name string) *tar.Header {
			return &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o600, Uid: 5}
		}
		for _, name := range []string{"four/tool", "five/tool"} {
			if err := c.WriteRegularFromPathDeduplicated(withMeta(name), pathA); err != nil {
				t.Fatal(err)
			}
		}
	})

	checkReconstructs(t, l)

	for _, name := range []string{"one/tool", "two/tool", "three/tool", "four/tool", "five/tool"} {
		if got := l.read(t, name); got != content {
			t.Errorf("%s content does not match", name)
		}
	}
	if st := l.stat(t, "one/tool"); st.Nlink != 3 {
		t.Errorf("one/tool nlink %d, want 3", st.Nlink)
	}
	if st := l.stat(t, "four/tool"); st.Nlink != 2 {
		t.Errorf("four/tool nlink %d, want 2", st.Nlink)
	}
	if a, b := l.stat(t, "one/tool"), l.stat(t, "four/tool"); a.Ino == b.Ino {
		t.Error("entries with different metadata share an inode")
	}
	if a, b := l.stat(t, "four/tool"), l.stat(t, "five/tool"); a.Ino != b.Ino {
		t.Error("entries with identical metadata and content do not share an inode")
	}
}

// TestWhiteoutTranslation checks that AUFS markers become overlayfs whiteouts
// and never appear as ".wh." names in the image.
func TestWhiteoutTranslation(t *testing.T) {
	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if err := c.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "etc/", Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		// Whiteout markers are ordinary zero-size regular files in a tar
		// layer, so they arrive through the regular-file paths.
		if err := c.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/.wh.gone", Mode: 0o644}, nil); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteRegularDeduplicated(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/.wh..wh..opq", Mode: 0o644}, nil); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteRegularFromPath(&tar.Header{Typeflag: tar.TypeReg, Name: ".wh.toplevel", Mode: 0o644}, "/nonexistent"); err != nil {
			t.Fatal(err)
		}
	})

	efs := l.open(t)
	if err := fs.WalkDir(efs, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".wh.") {
			t.Errorf("image contains an AUFS whiteout name: %s", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"etc/gone", "toplevel"} {
		st := l.stat(t, p)
		if st.Mode&fs.ModeCharDevice == 0 {
			t.Errorf("%s mode %v, want a character device", p, st.Mode)
		}
		if st.Rdev != 0 {
			t.Errorf("%s rdev %d, want 0", p, st.Rdev)
		}
		// mkfs.erofs keeps the marker's permission bits and replaces only its
		// type, so the two producers agree.
		if st.Mode.Perm() != 0o644 {
			t.Errorf("%s mode %v, want the marker's 0644", p, st.Mode.Perm())
		}
	}
	if st := l.stat(t, "etc"); st.Xattrs[OverlayOpaqueXattr] != "y" {
		t.Errorf("etc xattrs %v, want %s=y", st.Xattrs, OverlayOpaqueXattr)
	}
}

// TestWhiteoutOpaqueDoesNotClobberDirectoryMode checks that an opaque marker
// arriving after its directory's own entry leaves that entry's permissions
// alone.
func TestWhiteoutOpaqueDoesNotClobberDirectoryMode(t *testing.T) {
	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if err := c.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "etc/", Mode: 0o700, Uid: 9}); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/.wh..wh..opq", Mode: 0o644}, nil); err != nil {
			t.Fatal(err)
		}
	})
	st := l.stat(t, "etc")
	if st.Mode.Perm() != 0o700 || st.UID != 9 {
		t.Errorf("etc = %v uid %d, want 0700 uid 9", st.Mode.Perm(), st.UID)
	}
	if st.Xattrs[OverlayOpaqueXattr] != "y" {
		t.Errorf("etc is not marked opaque: %v", st.Xattrs)
	}
}

// TestImportedHardlink checks that a tar hardlink becomes a real EROFS
// hardlink, and that one pointing outside the layer fails loudly rather than
// producing a broken image.
func TestImportedHardlink(t *testing.T) {
	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if err := c.WriteRegular(regularHdr("bin/busybox", 4), strings.NewReader("bin\n")); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "bin/sh", Linkname: "bin/busybox"}); err != nil {
			t.Fatal(err)
		}
	})
	if a, b := l.stat(t, "bin/busybox"), l.stat(t, "bin/sh"); a.Ino != b.Ino {
		t.Error("hardlink does not share the target's inode")
	}

	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", "none", &blob)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSHA256CASWithDigestFS(appender, digestfs.New(SHA256Helper{}))
	err = c.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "bin/sh", Linkname: "lower/layer/file"})
	if err == nil {
		t.Fatal("a hardlink into a lower layer succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "across layers") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

// TestTreeArtifact checks that a directory input is recorded with its files, and
// that a repeated one can be deduplicated.
func TestTreeArtifact(t *testing.T) {
	tree := fstest.MapFS{
		"a.txt":       {Data: []byte("a\n")},
		"sub/b.txt":   {Data: []byte("b\n")},
		"sub/c/d.txt": {Data: []byte("d\n")},
	}

	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if _, err := c.StoreTree(tree, "opt/data"); err != nil {
			t.Fatal(err)
		}
	})
	checkReconstructs(t, l)

	for name, want := range map[string]string{
		"opt/data/a.txt":       "a\n",
		"opt/data/sub/b.txt":   "b\n",
		"opt/data/sub/c/d.txt": "d\n",
	} {
		if got := l.read(t, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestTreeArtifactMetadata checks that a layer's configured metadata reaches the
// entries of a stored tree, which arrive as a filesystem rather than as headers
// and so are only described by the GeneratedMetadata hook. tarcas honours the
// same hook, so the two sinks give a tree the same attributes.
func TestTreeArtifactMetadata(t *testing.T) {
	tree := fstest.MapFS{
		"a.txt":     {Data: []byte("a\n")},
		"sub/b.txt": {Data: []byte("b\n")},
	}
	mtime := time.Date(2023, 1, 1, 8, 0, 0, 0, time.UTC)
	apply := func(hdr *tar.Header) error {
		hdr.Uid = 42
		hdr.Gid = 43
		hdr.ModTime = mtime
		if hdr.Typeflag != tar.TypeDir {
			hdr.Mode = 0o640
		}
		if hdr.Name == "opt/data/sub/b.txt" {
			hdr.Mode = 0o600
		}
		return nil
	}

	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if _, err := c.StoreTree(tree, "opt/data"); err != nil {
			t.Fatal(err)
		}
	}, GeneratedMetadata(apply), DeduplicateTreeArtifacts(true))
	checkReconstructs(t, l)

	for name, wantMode := range map[string]fs.FileMode{
		"opt/data":           0o755,
		"opt/data/a.txt":     0o640,
		"opt/data/sub/b.txt": 0o600,
	} {
		st := l.stat(t, name)
		if st.Mode.Perm() != wantMode {
			t.Errorf("%s mode %v, want %v", name, st.Mode.Perm(), wantMode)
		}
		if st.UID != 42 || st.GID != 43 {
			t.Errorf("%s owner %d:%d, want 42:43", name, st.UID, st.GID)
		}
		if got := time.Unix(int64(st.Mtime), 0).UTC(); !got.Equal(mtime) {
			t.Errorf("%s mtime %s, want %s", name, got, mtime)
		}
	}

	// "sub" is invented by the tree walk rather than described, so it keeps the
	// synthesized-directory attributes instead of the tree's metadata.
	if got := l.read(t, "opt/data/sub/b.txt"); got != "b\n" {
		t.Errorf("content changed: %q", got)
	}
}

// TestReplacedPathLastWins checks that a second entry at the same path replaces
// the first, which is what extracting a tar with duplicate paths would do.
func TestReplacedPathLastWins(t *testing.T) {
	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if err := c.WriteRegular(regularHdr("etc/motd", 6), strings.NewReader("first\n")); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteRegular(regularHdr("etc/motd", 7), strings.NewReader("second\n")); err != nil {
			t.Fatal(err)
		}
	})
	if got := l.read(t, "etc/motd"); got != "second\n" {
		t.Errorf("etc/motd = %q, want %q", got, "second\n")
	}
}

// TestReplacingADeduplicationTargetFails checks that overwriting the entry other
// entries were hardlinked to is reported instead of silently changing what those
// links resolve to.
func TestReplacingADeduplicationTargetFails(t *testing.T) {
	content := strings.Repeat("dup\n", 2000)
	p := writeTempFile(t, "dup.bin", content)

	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", "none", &blob)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSHA256CASWithDigestFS(appender, digestfs.New(SHA256Helper{}))
	for _, name := range []string{"one", "two"} {
		if err := c.WriteRegularFromPathDeduplicated(regularHdr(name, int64(len(content))), p); err != nil {
			t.Fatal(err)
		}
	}
	err = c.WriteRegular(regularHdr("one", 4), strings.NewReader("new\n"))
	if err == nil {
		t.Fatal("replacing a deduplication target succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "recorded twice") {
		t.Errorf("error does not explain the problem: %v", err)
	}
}

// TestContentManifestRoundTrip checks that the hashes exported for reuse across
// layers survive a round trip.
func TestContentManifestRoundTrip(t *testing.T) {
	content := strings.Repeat("manifest\n", 600)
	p := writeTempFile(t, "m.bin", content)

	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", "none", &blob)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSHA256CASWithDigestFS(appender, digestfs.New(SHA256Helper{}))
	if err := c.WriteRegularFromPathDeduplicated(regularHdr("blob", int64(len(content))), p); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRegularFromPathDeduplicated(&tar.Header{
		Typeflag: tar.TypeReg, Name: "node", Size: int64(len(content)), Mode: 0o600,
	}, p); err != nil {
		t.Fatal(err)
	}

	var exported collectedState
	if err := c.Export(&exported); err != nil {
		t.Fatal(err)
	}
	if len(exported.blobs) != 1 || len(exported.nodes) != 1 {
		t.Fatalf("exported %d blob and %d node hashes, want 1 and 1", len(exported.blobs), len(exported.nodes))
	}

	// Importing them into a fresh sink records them as already known.
	c2 := NewSHA256CASWithDigestFS(io.Discard, digestfs.New(SHA256Helper{}))
	if err := c2.Import(&exported); err != nil {
		t.Fatal(err)
	}
	var reexported collectedState
	if err := c2.Export(&reexported); err != nil {
		t.Fatal(err)
	}
	// Import records hashes but does not re-export them, exactly as tarcas
	// behaves: cross-layer deduplication is a separate concern.
	if len(reexported.blobs) != 0 {
		t.Errorf("a sink that only imported re-exported %d hashes, want 0", len(reexported.blobs))
	}
}

// collectedState collects exported CAS hashes and hands them back on import.
type collectedState struct {
	blobs, nodes, trees [][]byte
}

func (s *collectedState) Export(from api.CASStateSupplier) error {
	for h, err := range from.BlobHashes() {
		if err != nil {
			return err
		}
		s.blobs = append(s.blobs, h)
	}
	for h, err := range from.NodeHashes() {
		if err != nil {
			return err
		}
		s.nodes = append(s.nodes, h)
	}
	for h, err := range from.TreeHashes() {
		if err != nil {
			return err
		}
		s.trees = append(s.trees, h)
	}
	return nil
}

func (s *collectedState) BlobHashes() iter.Seq2[[]byte, error] { return sequence(s.blobs) }
func (s *collectedState) NodeHashes() iter.Seq2[[]byte, error] { return sequence(s.nodes) }
func (s *collectedState) TreeHashes() iter.Seq2[[]byte, error] { return sequence(s.trees) }

// TestCorruptedPlanFailsAtBuildTime checks the splitter's verification: if a
// recorded digest does not describe the bytes that land in the region, the layer
// action fails rather than emitting a compact stream that cannot be
// reconstructed.
func TestCorruptedPlanFailsAtBuildTime(t *testing.T) {
	content := strings.Repeat("verify\n", 2000)
	p := writeTempFile(t, "v.bin", content)

	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", "none", &blob)
	if err != nil {
		t.Fatal(err)
	}
	var cs bytes.Buffer
	csWriter := compactstream.NewWriter(&cs, compactstream.HashAlgoSHA256, sha256.Size,
		compactstream.StreamCompressionZstd,
		compactstream.OriginalCompressionInfo{CompressionLevel: -1, CompressorJobs: 1}, 0)

	c := NewSHA256CASWithDigestFS(appender, digestfs.New(SHA256Helper{}),
		WithCompactStreamWriter{Writer: csWriter})
	if err := c.WriteRegularFromPath(regularHdr("f", int64(len(content))), p); err != nil {
		t.Fatal(err)
	}
	// Corrupt the recorded digest behind the sink's back, as a layout or digest
	// bug would.
	if err := c.w.SetToken("/f", bytes.Repeat([]byte{0xAB}, sha256.Size)); err != nil {
		t.Fatal(err)
	}

	err = c.Close()
	if err == nil {
		t.Fatal("Close succeeded with a wrong digest, want an error")
	}
	if !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("error does not report the digest mismatch: %v", err)
	}
}

// TestDeterministic checks that recording the same layer twice produces
// identical bytes.
func TestDeterministic(t *testing.T) {
	content := strings.Repeat("determinism\n", 500)
	p := writeTempFile(t, "d.bin", content)

	build := func() ([]byte, []byte) {
		l := buildLayer(t, "zstd", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
			store.put([]byte(content))
			if err := c.WriteRegularFromPathDeduplicated(regularHdr("b/tool", int64(len(content))), p); err != nil {
				t.Fatal(err)
			}
			if err := c.WriteRegularFromPathDeduplicated(regularHdr("a/tool", int64(len(content))), p); err != nil {
				t.Fatal(err)
			}
			if err := c.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "a/tool"}); err != nil {
				t.Fatal(err)
			}
			if err := c.WriteRegular(&tar.Header{
				Typeflag: tar.TypeReg, Name: "x", Size: 2, Mode: 0o644,
				PAXRecords: map[string]string{"SCHILY.xattr.user.b": "2", "SCHILY.xattr.user.a": "1"},
			}, strings.NewReader("x\n")); err != nil {
				t.Fatal(err)
			}
		})
		return l.blob, l.compactStrm
	}

	blobA, csA := build()
	blobB, csB := build()
	if !bytes.Equal(blobA, blobB) {
		t.Error("two identical builds produced different blobs")
	}
	if !bytes.Equal(csA, csB) {
		t.Error("two identical builds produced different compact streams")
	}
}

// TestInlineThresholdMovesPayloadsIntoTheStream checks that the threshold
// decides whether a small file's bytes ride along in the compact stream or
// become a CAS reference.
func TestInlineThresholdMovesPayloadsIntoTheStream(t *testing.T) {
	content := "0123456789"
	p := writeTempFile(t, "s.bin", content)

	refCount := func(opts ...Option) int {
		l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
			store.put([]byte(content))
			if err := c.WriteRegularFromPath(regularHdr("f", int64(len(content))), p); err != nil {
				t.Fatal(err)
			}
		}, opts...)
		checkReconstructs(t, l)
		info, err := compactstream.Inspect(bytes.NewReader(l.compactStrm))
		if err != nil {
			t.Fatal(err)
		}
		if info.Header.ContentFormat != compactstream.ContentFormatErofs {
			t.Errorf("content format %d, want %d (erofs)", info.Header.ContentFormat, compactstream.ContentFormatErofs)
		}
		return len(info.Refs)
	}

	if got := refCount(); got != 0 {
		t.Errorf("with tail packing: %d CAS refs, want 0 (the payload rides inline)", got)
	}
	if got := refCount(InlineThreshold(4)); got != 1 {
		t.Errorf("with a 4-byte inline threshold: %d CAS refs, want 1", got)
	}
}

// TestSubSecondMtimeIsPreserved checks that a timestamp with sub-second
// precision survives into the image. Normalizing a header rounds its timestamp
// to whole seconds so that the content hash is stable, but EROFS stores
// nanoseconds and the layer should record what the caller asked for.
func TestSubSecondMtimeIsPreserved(t *testing.T) {
	mtime := time.Unix(1700000000, 600_000_000).UTC()
	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if err := c.WriteRegular(&tar.Header{
			Typeflag: tar.TypeReg, Name: "f", Size: 2, Mode: 0o644, ModTime: mtime,
		}, strings.NewReader("f\n")); err != nil {
			t.Fatal(err)
		}
		if err := c.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: "d/", Mode: 0o755, ModTime: mtime,
		}); err != nil {
			t.Fatal(err)
		}
	})

	for _, p := range []string{"f", "d"} {
		st := l.stat(t, p)
		if st.Mtime != uint64(mtime.Unix()) || st.MtimeNs != uint32(mtime.Nanosecond()) {
			t.Errorf("%s mtime %d.%09d, want %d.%09d", p, st.Mtime, st.MtimeNs, mtime.Unix(), mtime.Nanosecond())
		}
	}
}

// TestOpaqueWhiteoutOnUndescribedDirectory checks that a directory which exists
// only because an opaque marker needed something to attach to is treated as
// invented rather than described: its attributes come from the configured policy
// and it is reported, so strict mode catches it.
func TestOpaqueWhiteoutOnUndescribedDirectory(t *testing.T) {
	// The default inherit policy gives it the nearest described ancestor's
	// ownership, like every other invented directory.
	l := buildLayer(t, "none", func(t *testing.T, c *CAS[SHA256Helper], store memBlobStore) {
		if err := c.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: "opt/", Mode: 0o750, Uid: 70, Gid: 70,
		}); err != nil {
			t.Fatal(err)
		}
		// No entry of its own for opt/data: only the marker.
		if err := c.WriteRegular(&tar.Header{
			Typeflag: tar.TypeReg, Name: "opt/data/.wh..wh..opq", Mode: 0o644,
		}, nil); err != nil {
			t.Fatal(err)
		}
	})

	st := l.stat(t, "opt/data")
	if st.UID != 70 || st.GID != 70 {
		t.Errorf("opt/data owner %d:%d, want 70:70 (inherited)", st.UID, st.GID)
	}
	if st.Xattrs[OverlayOpaqueXattr] != "y" {
		t.Errorf("opt/data is not marked opaque: %v", st.Xattrs)
	}

	// Strict mode must refuse to guess it.
	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", "none", &blob)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSHA256CASWithDigestFS(appender, digestfs.New(SHA256Helper{}),
		WithSyntheticDirectories(SyntheticDirectoriesStrict))
	if err := c.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "opt/", Mode: 0o750}); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteRegular(&tar.Header{
		Typeflag: tar.TypeReg, Name: "opt/data/.wh..wh..opq", Mode: 0o644,
	}, nil); err != nil {
		t.Fatal(err)
	}
	err = c.Close()
	if err == nil {
		t.Fatal("Close succeeded, want strict mode to refuse the invented directory")
	}
	if !strings.Contains(err.Error(), "/opt/data") {
		t.Errorf("error does not name the invented directory: %v", err)
	}
}

// TestOpaqueWhiteoutDirectoryDescribedLater checks that describing the directory
// after the marker takes it out of the report.
func TestOpaqueWhiteoutDirectoryDescribedLater(t *testing.T) {
	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", "none", &blob)
	if err != nil {
		t.Fatal(err)
	}
	c := NewSHA256CASWithDigestFS(appender, digestfs.New(SHA256Helper{}),
		WithSyntheticDirectories(SyntheticDirectoriesStrict))
	if err := c.WriteRegular(&tar.Header{
		Typeflag: tar.TypeReg, Name: "opt/.wh..wh..opq", Mode: 0o644,
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "opt/", Mode: 0o750, Uid: 9}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	l := layer{blob: blob.Bytes(), image: blob.Bytes()}
	st := l.stat(t, "opt")
	if st.Mode.Perm() != 0o750 || st.UID != 9 {
		t.Errorf("opt = %v uid %d, want 0750 uid 9", st.Mode.Perm(), st.UID)
	}
	if st.Xattrs[OverlayOpaqueXattr] != "y" {
		t.Errorf("opt lost its opaque marker: %v", st.Xattrs)
	}
}
