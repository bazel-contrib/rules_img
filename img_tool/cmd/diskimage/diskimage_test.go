package diskimage

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/disk"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/ext4/ext4cstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/fat32/fat32cstream"
)

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// fixtures are file system images in the forms a partition can come in, and the
// content-addressed directory that holds the blobs the compact streams
// reference.
type fixtures struct {
	dir                         string
	espImage, espStream         string
	rootImage, rootStream       string
	casDir                      string
	espSize, rootSize           int64
	bootloader, bigFile, config []byte
}

func writeBlob(t *testing.T, casDir string, content []byte) {
	t.Helper()
	sum := sha256.Sum256(content)
	p := filepath.Join(casDir, "sha256", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixtures(t *testing.T) *fixtures {
	t.Helper()
	f := &fixtures{
		dir:        t.TempDir(),
		bootloader: pattern(150_000, 1),
		bigFile:    pattern(3<<20+17, 2),
		config:     []byte("root=PARTLABEL=root\n"),
	}
	f.casDir = filepath.Join(f.dir, "cas")

	// A FAT32 ESP, as image and as compact stream.
	esp := func(path string, stream string) {
		b, err := fat32.NewBuilder(fat32.Options{DigestFiles: true})
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		add := func(name string, content []byte) {
			if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644}, bytes.NewReader(content)); err != nil {
				t.Fatal(err)
			}
		}
		add("EFI/BOOT/BOOTX64.EFI", f.bootloader)
		add("loader/loader.conf", f.config)
		img, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer img.Close()
		out, err := os.Create(stream)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		info, err := fat32cstream.Build(b, img, out, fat32cstream.Options{})
		if err != nil {
			t.Fatal(err)
		}
		f.espSize = info.Size
	}
	f.espImage, f.espStream = filepath.Join(f.dir, "esp.fat32"), filepath.Join(f.dir, "esp.cstream")
	esp(f.espImage, f.espStream)
	writeBlob(t, f.casDir, f.bootloader)

	// An ext4 root file system.
	f.rootImage, f.rootStream = filepath.Join(f.dir, "root.ext4"), filepath.Join(f.dir, "root.cstream")
	b, err := ext4.NewBuilder(ext4.Options{DigestFiles: true})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for name, content := range map[string][]byte{"usr/lib/big": f.bigFile, "etc/os-release": []byte("NAME=test\n")} {
		if err := b.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644}, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	img, err := os.Create(f.rootImage)
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	out, err := os.Create(f.rootStream)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	info, err := ext4cstream.Build(b, img, out, ext4cstream.Options{})
	if err != nil {
		t.Fatal(err)
	}
	f.rootSize = info.Size
	writeBlob(t, f.casDir, f.bigFile)
	return f
}

func (f *fixtures) cfg(t *testing.T, output string, partitions ...*partitionConfig) *config {
	return &config{
		table: "gpt", sectorSize: 512, alignment: "1MiB", inlineThreshold: "1MiB",
		partitions: partitions, output: filepath.Join(f.dir, output),
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// reconstructDigest reconstructs a compact stream and returns the digest and
// the size of the result.
func reconstructDigest(t *testing.T, stream string, casDirs ...string) (string, int64) {
	t.Helper()
	fh, err := os.Open(stream)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	h := sha256.New()
	n, err := countingCopy(h, fh, casDirs)
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil)), n
}

func countingCopy(h io.Writer, stream io.Reader, casDirs []string) (int64, error) {
	r, err := compactstream.NewReconstructingReader(context.Background(), stream, compactstream.NewDirStore(casDirs...))
	if err != nil {
		return 0, err
	}
	defer r.Close()
	return io.Copy(h, r)
}

func readAt(t *testing.T, path string, off, n int64) []byte {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	b := make([]byte, n)
	if _, err := fh.ReadAt(b, off); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return b
}

func readLayout(t *testing.T, path string) layoutJSON {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var l layoutJSON
	if err := json.Unmarshal(data, &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRawDisk(t *testing.T) {
	f := newFixtures(t)
	cfg := f.cfg(t, "disk.img",
		&partitionConfig{label: "ESP", typ: "esp", image: f.espImage},
		&partitionConfig{label: "scratch", typ: "linux", size: "4MiB"},
		&partitionConfig{label: "root", typ: "root-x86-64", image: f.rootImage, size: "64MiB", attrs: []string{"required"}},
	)
	cfg.layoutJSON = filepath.Join(f.dir, "layout.json")
	if err := run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	layout := readLayout(t, cfg.layoutJSON)
	st, err := os.Stat(cfg.output)
	if err != nil || st.Size() != layout.Size {
		t.Fatalf("disk is %v bytes, %v; layout says %d", st.Size(), err, layout.Size)
	}
	if len(layout.Partitions) != 3 || layout.Partitions[0].Filesystem != "fat32" || layout.Partitions[2].Filesystem != "ext4" || layout.Partitions[1].Filesystem != "" {
		t.Fatalf("layout: %+v", layout.Partitions)
	}

	// The root file system is in its partition unchanged, followed by free space.
	root := layout.Partitions[2]
	if root.Length != 64<<20 || root.ContentSize != f.rootSize {
		t.Errorf("root partition: %+v", root)
	}
	want, _ := os.ReadFile(f.rootImage)
	if got := readAt(t, cfg.output, root.Offset, f.rootSize); !bytes.Equal(got, want) {
		t.Error("the ext4 image is not in its partition")
	}
	if free := readAt(t, cfg.output, root.Offset+f.rootSize, root.Length-f.rootSize); !bytes.Equal(free, make([]byte, len(free))) {
		t.Error("the free space of the partition is not zero")
	}

	// The ESP differs from the image in the hidden sectors of the boot sector and of its backup.
	esp := layout.Partitions[0]
	espWant, _ := os.ReadFile(f.espImage)
	got := readAt(t, cfg.output, esp.Offset, f.espSize)
	hidden := uint32(esp.StartLBA)
	for _, off := range []int{28, 6*512 + 28} {
		if binary.LittleEndian.Uint32(got[off:]) != hidden || binary.LittleEndian.Uint32(espWant[off:]) != 0 {
			t.Errorf("hidden sectors at %d: %d, want %d", off, binary.LittleEndian.Uint32(got[off:]), hidden)
		}
		binary.LittleEndian.PutUint32(got[off:], 0)
	}
	if !bytes.Equal(got, espWant) {
		t.Error("the FAT32 image differs from its partition by more than the hidden sectors")
	}

	// The table in front and behind is what the disk package computes.
	l, err := disk.Plan(disk.Spec{Partitions: []disk.Partition{
		{Label: "ESP", Type: "esp", ContentSize: f.espSize},
		{Label: "scratch", Type: "linux", Size: 4 << 20},
		{Label: "root", Type: "root-x86-64", ContentSize: f.rootSize, Size: 64 << 20, Attributes: disk.AttrRequired},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if head := l.Head(); !bytes.Equal(readAt(t, cfg.output, 0, int64(len(head))), head) {
		t.Error("the partition table differs")
	}
	if tail, off := l.Tail(); !bytes.Equal(readAt(t, cfg.output, off, int64(len(tail))), tail) {
		t.Error("the backup partition table differs")
	}
}

// TestCompactDiskEqualsRawDisk is the defining property of the compact mode:
// the compact stream reconstructs to the disk image of the raw mode, whatever
// form the inputs have.
func TestCompactDiskEqualsRawDisk(t *testing.T) {
	f := newFixtures(t)
	type variant struct {
		name  string
		table string
		parts func() []*partitionConfig
	}
	for _, v := range []variant{
		{"images", "gpt", func() []*partitionConfig {
			return []*partitionConfig{
				{label: "ESP", typ: "esp", image: f.espImage},
				{label: "root", typ: "linux", image: f.rootImage},
			}
		}},
		{"streams", "gpt", func() []*partitionConfig {
			return []*partitionConfig{
				{label: "ESP", typ: "esp", cstream: f.espStream},
				{label: "root", typ: "linux", cstream: f.rootStream},
			}
		}},
		{"both", "gpt", func() []*partitionConfig {
			return []*partitionConfig{
				{label: "ESP", typ: "esp", image: f.espImage, cstream: f.espStream},
				{label: "gap", typ: "linux", size: "3MiB"},
				{label: "root", typ: "linux", image: f.rootImage, cstream: f.rootStream, size: "50MiB"},
				{label: "free", typ: "swap", size: "10MiB"},
			}
		}},
		{"mbr", "mbr", func() []*partitionConfig {
			return []*partitionConfig{
				{label: "ESP", typ: "esp", cstream: f.espStream, active: true},
				{label: "root", typ: "linux", image: f.rootImage},
			}
		}},
		{"mixed", "gpt", func() []*partitionConfig {
			return []*partitionConfig{
				{label: "ESP", typ: "esp", image: f.espImage},
				{label: "root", typ: "linux", cstream: f.rootStream},
			}
		}},
	} {
		t.Run(v.name, func(t *testing.T) {
			raw := f.cfg(t, v.name+".raw.img", v.parts()...)
			raw.table = v.table
			raw.casDirs = []string{f.casDir}
			raw.minSize = "100MiB"
			if err := run(context.Background(), raw); err != nil {
				t.Fatal(err)
			}

			casOut := filepath.Join(f.dir, v.name+".cas")
			compact := f.cfg(t, v.name+".cstream", v.parts()...)
			compact.table = v.table
			compact.compact = true
			compact.minSize = "100MiB"
			compact.outputCASDir = casOut
			compact.layoutJSON = filepath.Join(f.dir, v.name+".layout.json")
			if err := run(context.Background(), compact); err != nil {
				t.Fatal(err)
			}

			gotDigest, gotSize := reconstructDigest(t, compact.output, casOut, f.casDir)
			st, _ := os.Stat(raw.output)
			if gotSize != st.Size() || gotDigest != fileDigest(t, raw.output) {
				t.Errorf("the compact stream reconstructs to %d bytes (%s), the raw disk has %d bytes (%s)", gotSize, gotDigest, st.Size(), fileDigest(t, raw.output))
			}
			if !readLayout(t, compact.layoutJSON).Compact {
				t.Error("the layout does not say that the output is compact")
			}
		})
	}
}

// TestCompactInheritsReferences checks what the compact stream of the disk is
// made of: the files of the file systems are references, the rest is inline.
func TestCompactInheritsReferences(t *testing.T) {
	f := newFixtures(t)
	cfg := f.cfg(t, "disk.cstream",
		&partitionConfig{label: "ESP", typ: "esp", image: f.espImage, cstream: f.espStream},
		&partitionConfig{label: "root", typ: "linux", image: f.rootImage, cstream: f.rootStream},
	)
	cfg.compact = true
	cfg.minSize = "2GiB"
	cfg.outputCASDir = filepath.Join(f.dir, "cas-out")
	if err := run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	fh, err := os.Open(cfg.output)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	info, err := compactstream.Inspect(fh)
	if err != nil {
		t.Fatal(err)
	}
	digests := map[string]int64{}
	for _, r := range info.Refs {
		digests[hex.EncodeToString(r.Digest)] = int64(r.Size)
	}
	bigSum, bootSum := sha256.Sum256(f.bigFile), sha256.Sum256(f.bootloader)
	if digests[hex.EncodeToString(bigSum[:])] != int64(len(f.bigFile)) || digests[hex.EncodeToString(bootSum[:])] != int64(len(f.bootloader)) {
		t.Errorf("the files of the file systems are not references: %v", digests)
	}
	if len(info.Refs) != 2 {
		t.Errorf("%d references, want the two large files", len(info.Refs))
	}
	if st, _ := os.Stat(cfg.output); st.Size() > 1<<20 {
		t.Errorf("the compact stream of a 2 GiB disk is %d bytes", st.Size())
	}
	if info.ReconstructedSize() != 2<<30 {
		t.Errorf("reconstructs to %d bytes", info.ReconstructedSize())
	}
	// Nothing had to be put in the CAS: no image was referenced as a whole.
	if _, err := os.Stat(cfg.outputCASDir); err == nil {
		t.Error("an output CAS directory was created")
	}
}

func TestCompactReferencesLargeImages(t *testing.T) {
	f := newFixtures(t)
	for _, symlink := range []bool{false, true} {
		cfg := f.cfg(t, fmt.Sprintf("disk-%v.cstream", symlink), &partitionConfig{label: "root", typ: "linux", image: f.rootImage})
		cfg.compact = true
		cfg.inlineThreshold = "1MiB"
		cfg.outputCASDir = filepath.Join(f.dir, fmt.Sprintf("cas-%v", symlink))
		cfg.casSymlink = symlink
		if err := run(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		fh, _ := os.Open(cfg.output)
		info, err := compactstream.Inspect(fh)
		fh.Close()
		if err != nil || len(info.Refs) != 1 || info.Refs[0].Size != uint64(f.rootSize) {
			t.Fatalf("references: %+v, %v", info.Refs, err)
		}
		blob := filepath.Join(cfg.outputCASDir, "sha256", hex.EncodeToString(info.Refs[0].Digest))
		if fileDigest(t, blob) != hex.EncodeToString(info.Refs[0].Digest) {
			t.Errorf("the CAS entry (symlink %v) is not the image", symlink)
		}
		if _, err := os.Lstat(blob); err != nil {
			t.Fatal(err)
		}
		// The stream reconstructs to a disk that holds the image.
		raw := f.cfg(t, fmt.Sprintf("raw-%v.img", symlink), &partitionConfig{label: "root", typ: "linux", image: f.rootImage})
		if err := run(context.Background(), raw); err != nil {
			t.Fatal(err)
		}
		if got, _ := reconstructDigest(t, cfg.output, cfg.outputCASDir); got != fileDigest(t, raw.output) {
			t.Errorf("symlink %v: the compact stream does not reconstruct to the raw disk", symlink)
		}

		// Without a place for the image, it cannot be referenced.
		cfg.outputCASDir = ""
		if err := run(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "output-cas-dir") {
			t.Errorf("no output CAS directory: %v", err)
		}
		// A high threshold keeps the image inline.
		cfg.inlineThreshold = "1GiB"
		if err := run(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		fh, _ = os.Open(cfg.output)
		info, _ = compactstream.Inspect(fh)
		fh.Close()
		if len(info.Refs) != 0 {
			t.Errorf("%d references with a high threshold", len(info.Refs))
		}
	}
}

func TestSectorSize4096(t *testing.T) {
	f := newFixtures(t)
	// An ext4 image works with any sector size, a FAT32 image needs to match.
	cfg := f.cfg(t, "disk4k.img", &partitionConfig{label: "root", typ: "linux", image: f.rootImage})
	cfg.sectorSize = 4096
	cfg.layoutJSON = filepath.Join(f.dir, "layout4k.json")
	if err := run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	layout := readLayout(t, cfg.layoutJSON)
	if layout.SectorSize != 4096 || layout.FirstUsable != 6 || layout.Partitions[0].Offset != 1<<20 {
		t.Errorf("layout: %+v", layout)
	}

	cfg = f.cfg(t, "disk-esp.img", &partitionConfig{label: "ESP", typ: "esp", image: f.espImage})
	cfg.sectorSize = 4096
	err := run(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "512 byte sectors") || !strings.Contains(err.Error(), "4096 byte sectors") {
		t.Errorf("a FAT32 file system with 512 byte sectors on a disk with 4096 byte sectors: %v", err)
	}
}

func TestErrors(t *testing.T) {
	f := newFixtures(t)
	ctx := context.Background()
	for name, cfg := range map[string]*config{
		"ESP that is not FAT32":      f.cfg(t, "x.img", &partitionConfig{label: "ESP", typ: "esp", image: f.rootImage}),
		"placeholder without a size": f.cfg(t, "x.img", &partitionConfig{label: "empty", typ: "linux"}),
		"unknown type":               f.cfg(t, "x.img", &partitionConfig{label: "x", typ: "ntfs-ish", image: f.rootImage}),
		"bad GUID":                   f.cfg(t, "x.img", &partitionConfig{label: "x", guid: "nope", image: f.rootImage}),
		"bad size":                   f.cfg(t, "x.img", &partitionConfig{label: "x", size: "lots", image: f.rootImage}),
		"bad attribute":              f.cfg(t, "x.img", &partitionConfig{label: "x", attrs: []string{"sticky"}, image: f.rootImage}),
		"missing image":              f.cfg(t, "x.img", &partitionConfig{label: "x", image: filepath.Join(f.dir, "missing")}),
		"too small":                  f.cfg(t, "x.img", &partitionConfig{label: "x", image: f.rootImage, size: "1MiB"}),
		"missing boot code":          func() *config { c := f.cfg(t, "x.img"); c.bootCode = filepath.Join(f.dir, "none"); return c }(),
	} {
		if err := run(ctx, cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// A compact stream partition in a raw disk needs its blobs.
	cfg := f.cfg(t, "x.img", &partitionConfig{label: "root", typ: "linux", cstream: f.rootStream})
	if err := run(ctx, cfg); err == nil || !strings.Contains(err.Error(), "not found in content-addressed directory") {
		t.Errorf("missing blobs: %v", err)
	}
	cfg.casDirs = []string{f.casDir}
	if err := run(ctx, cfg); err != nil {
		t.Errorf("with the blobs: %v", err)
	}

	// The disk size limits.
	cfg = f.cfg(t, "x.img", &partitionConfig{label: "root", typ: "linux", image: f.rootImage})
	cfg.maxSize = "1MiB"
	if err := run(ctx, cfg); err == nil || !strings.Contains(err.Error(), "maximum disk size") {
		t.Errorf("max size: %v", err)
	}
}

func TestBootCodeAndMBR(t *testing.T) {
	f := newFixtures(t)
	code := filepath.Join(f.dir, "boot.bin")
	os.WriteFile(code, []byte("\xeb\x3c\x90 boot code"), 0o644)
	cfg := f.cfg(t, "mbr.img", &partitionConfig{label: "ESP", typ: "esp", image: f.espImage, active: true})
	cfg.table = "mbr"
	cfg.bootCode = code
	if err := run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	sector := readAt(t, cfg.output, 0, 512)
	if !bytes.HasPrefix(sector, []byte("\xeb\x3c\x90 boot code")) || sector[510] != 0x55 || sector[511] != 0xAA {
		t.Errorf("first sector: % x", sector[:32])
	}
	if e := sector[446:462]; e[0] != 0x80 || e[4] != 0xEF || binary.LittleEndian.Uint32(e[8:]) != 2048 {
		t.Errorf("partition entry: % x", e)
	}
}

func TestSniff(t *testing.T) {
	ext := make([]byte, 2048)
	binary.LittleEndian.PutUint16(ext[1024+0x38:], 0xEF53)
	erofs := make([]byte, 2048)
	binary.LittleEndian.PutUint32(erofs[1024:], 0xE0F5E1E2)
	fat := make([]byte, 512)
	copy(fat[82:], "FAT32   ")
	fat[510], fat[511] = 0x55, 0xAA
	for want, head := range map[fsKind][]byte{fsExt4: ext, fsErofs: erofs, fsFAT32: fat, fsUnknown: make([]byte, 4096)} {
		if got := sniff(head); got != want {
			t.Errorf("sniff = %s, want %s", got, want)
		}
	}
	if sniff(nil) != fsUnknown || sniff(make([]byte, 10)) != fsUnknown {
		t.Error("short input is not unknown")
	}
}
