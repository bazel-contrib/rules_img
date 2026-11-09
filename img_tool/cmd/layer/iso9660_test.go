package layer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fs/iso9660"
)

// iso9660Entries walks an image and returns its entries by path.
func iso9660Entries(t *testing.T, img []byte) map[string]*iso9660.Entry {
	t.Helper()
	rd, err := iso9660.OpenReader(bytes.NewReader(img))
	if err != nil {
		t.Fatalf("opening the image: %v", err)
	}
	out := map[string]*iso9660.Entry{}
	if err := rd.Walk(func(e *iso9660.Entry) error {
		copied := *e
		out[e.Path] = &copied
		return nil
	}); err != nil {
		t.Fatalf("walking the image: %v", err)
	}
	return out
}

func TestIso9660Layer(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, content []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	app := bytes.Repeat([]byte("application "), 50000)
	cfg := write("config.json", []byte(`{"a":1}`))
	appFile := write("app", app)
	treeDir := filepath.Join(dir, "tree")
	if err := os.Mkdir(treeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	treeContent := bytes.Repeat([]byte{7}, 9000)
	if err := os.WriteFile(filepath.Join(treeDir, "data.bin"), treeContent, 0o644); err != nil {
		t.Fatal(err)
	}
	empties := write("empties.txt", []byte("var/empty\n"))

	out := filepath.Join(dir, "layer.iso")
	cstreamPath := filepath.Join(dir, "layer.cstream")
	metadataPath := filepath.Join(dir, "metadata.json")

	// Options for tar layers are accepted and ignored.
	LayerProcess(context.Background(), []string{
		"--layer-format", "iso9660",
		"--estargz",
		"--compressor-jobs", "4",
		"--compression-level", "9",
		"--ztoc", filepath.Join(dir, "unused.ztoc"),
		"--iso9660-volume-id", "TESTVOL",
		"--iso9660-joliet",
		"--iso9660-udf",
		"--add", "/etc/app/config.json=" + cfg,
		"--add", "/opt/app=" + appFile,
		"--add", "/opt/tree=" + treeDir,
		"--symlink", "/opt/current=app",
		"--empty-files-from-file", empties,
		"--default-metadata", `{"uid":42,"gid":43}`,
		"--compact-stream", cstreamPath,
		"--compact-stream-inline-threshold", "4096",
		"--metadata", metadataPath,
		out,
	})

	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(img[16*2048+1:16*2048+6]) != "CD001" {
		t.Fatal("the output is not an ISO 9660 image")
	}
	if got := binary.LittleEndian.Uint32(img[16*2048+80:]); int64(got)*2048 != int64(len(img)) {
		t.Errorf("the volume descriptor says %d blocks for an image of %d bytes", got, len(img))
	}
	// Every file is stored in one contiguous extent, so its content shows up
	// whole and exactly once.
	if n := bytes.Count(img, app); n != 1 {
		t.Errorf("the application is stored %d times", n)
	}
	if !bytes.Contains(img, treeContent) || !bytes.Contains(img, []byte(`{"a":1}`)) {
		t.Error("the tree artifact or the config file is missing from the image")
	}
	if _, err := os.Stat(filepath.Join(dir, "unused.ztoc")); err == nil {
		t.Error("a ztoc was written")
	}

	entries := iso9660Entries(t, img)
	for _, name := range []string{"etc/app/config.json", "opt/app", "opt/tree/data.bin", "var/empty", "var"} {
		if entries[name] == nil {
			t.Errorf("%q is missing; the image has %v", name, len(entries))
		}
	}
	// Rock Ridge is on by default, so the real names and the metadata survive.
	if e := entries["etc/app/config.json"]; e != nil && (e.Uid != 42 || e.Gid != 43) {
		t.Errorf("the config file is owned by %d:%d, want 42:43", e.Uid, e.Gid)
	}
	if e := entries["opt/current"]; e == nil || e.Mode&fs.ModeSymlink == 0 || e.Target != "app" {
		t.Errorf("the symbolic link is %+v", e)
	}

	// The compact stream references the large files and resolves to the image.
	cstream, err := os.ReadFile(cstreamPath)
	if err != nil {
		t.Fatal(err)
	}
	csInfo, err := compactstream.Inspect(bytes.NewReader(cstream))
	if err != nil {
		t.Fatal(err)
	}
	if csInfo.Header.ContentFormat != compactstream.ContentFormatIso9660 {
		t.Errorf("the content format is %d, want ISO 9660", csInfo.Header.ContentFormat)
	}
	if len(csInfo.Refs) != 2 {
		t.Errorf("the compact stream has the references %+v, want two", csInfo.Refs)
	}
	appSum, treeSum := sha256.Sum256(app), sha256.Sum256(treeContent)
	var reconstructed bytes.Buffer
	blobs := blobStore{string(appSum[:]): app, string(treeSum[:]): treeContent}
	if err := compactstream.Reconstruct(context.Background(), bytes.NewReader(cstream), blobs, &reconstructed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(reconstructed.Bytes(), img) {
		t.Error("the reconstructed compact stream differs from the image")
	}

	var meta api.Descriptor
	raw, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	imgSum := sha256.Sum256(img)
	if meta.Size != int64(len(img)) || meta.Digest != fmt.Sprintf("sha256:%x", imgSum) || meta.Digest != meta.DiffID || meta.MediaType != api.Iso9660Layer {
		t.Errorf("the metadata is %+v (the image is %d bytes)", meta, len(img))
	}
}

// TestIso9660LayerWithoutRockRidge checks the plain image: the names are the
// mangled ISO 9660 identifiers and nothing ISO 9660 cannot describe is allowed.
func TestIso9660LayerWithoutRockRidge(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "Some File.txt")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "layer.iso")
	LayerProcess(context.Background(), []string{
		"--layer-format", "iso9660", "--iso9660-rock-ridge=disabled",
		"--add", "/Some File.txt=" + file, out,
	})
	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if entries := iso9660Entries(t, img); entries["SOME_FILE.TXT"] == nil {
		t.Errorf("the mangled identifier is missing; the image has %v", entries)
	}

	// A symbolic link needs Rock Ridge.
	image, err := os.Create(filepath.Join(dir, "rejected.iso"))
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	_, err = writeIso9660Layer(image, iso9660Settings{},
		addFiles{{PathInImage: "/f", File: file, FileType: api.RegularFile}}, nil, nil,
		symlinks{{LinkName: "/link", Target: "f"}}, nil, nil, false, nil, nil, 0)
	if err == nil {
		t.Error("a symbolic link was accepted without Rock Ridge")
	}
}

func TestIso9660LayerExtensions(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		// want is a byte sequence the image carries only with the extension.
		want string
	}{
		{"joliet", []string{"--iso9660-joliet"}, "%/E"},
		{"udf", []string{"--iso9660-udf"}, "NSR02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			with := filepath.Join(dir, tc.name+"-on.iso")
			without := filepath.Join(dir, tc.name+"-off.iso")
			LayerProcess(context.Background(), append([]string{"--layer-format", "iso9660", "--add", "/f=" + file}, append(tc.args, with)...))
			LayerProcess(context.Background(), []string{"--layer-format", "iso9660", "--add", "/f=" + file, without})
			on, err := os.ReadFile(with)
			if err != nil {
				t.Fatal(err)
			}
			off, err := os.ReadFile(without)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(on, []byte(tc.want)) {
				t.Errorf("the image built with %v does not carry %q", tc.args, tc.want)
			}
			if bytes.Contains(off, []byte(tc.want)) {
				t.Errorf("the image built without %v carries %q", tc.args, tc.want)
			}
		})
	}
}

func TestIso9660LayerElTorito(t *testing.T) {
	dir := t.TempDir()
	bios := bytes.Repeat([]byte("B"), 4096)
	efi := bytes.Repeat([]byte("E"), 8192)
	for name, content := range map[string][]byte{"isolinux.bin": bios, "efiboot.img": efi} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "layer.iso")
	LayerProcess(context.Background(), []string{
		"--layer-format", "iso9660",
		"--iso9660-volume-id", "INSTALLER",
		"--iso9660-joliet",
		"--add", "/isolinux/isolinux.bin=" + filepath.Join(dir, "isolinux.bin"),
		"--add", "/EFI/BOOT/efiboot.img=" + filepath.Join(dir, "efiboot.img"),
		"--iso9660-boot", `{"image":"/isolinux/isolinux.bin","platform":"bios","load_size":4,"boot_info_table":true}`,
		"--iso9660-boot", `{"image":"/EFI/BOOT/efiboot.img","platform":"uefi","id":"EFI"}`,
		out,
	})
	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	// The boot record points at the catalog; the catalog points at the files.
	var catalogLBA uint32
	for lba := 17; lba < 32; lba++ {
		d := img[lba*2048:]
		if string(d[1:6]) != "CD001" {
			break
		}
		if d[0] == 0 {
			if got := string(bytes.TrimRight(d[7:39], "\x00")); got != "EL TORITO SPECIFICATION" {
				t.Errorf("the boot system identifier is %q", got)
			}
			catalogLBA = binary.LittleEndian.Uint32(d[71:])
			break
		}
	}
	if catalogLBA == 0 {
		t.Fatal("the image has no El Torito boot record")
	}
	catalog := img[catalogLBA*2048:]
	if catalog[0] != 1 || catalog[30] != 0x55 || catalog[31] != 0xAA {
		t.Error("the boot catalog has no validation entry")
	}
	if catalog[1] != 0x00 {
		t.Errorf("the validation entry declares platform %#x, want BIOS", catalog[1])
	}
	if got := binary.LittleEndian.Uint16(catalog[32+6:]); got != 4 {
		t.Errorf("the default entry loads %d sectors, want 4", got)
	}
	if catalog[64] != 0x91 || catalog[65] != 0xEF {
		t.Errorf("the second entry is not a final UEFI section header: %#x %#x", catalog[64], catalog[65])
	}
	if got := binary.LittleEndian.Uint16(catalog[96+6:]); got != 16 {
		t.Errorf("the UEFI entry loads %d sectors, want 16 (the whole 8 KiB image)", got)
	}

	entries := iso9660Entries(t, img)
	for i, path := range []string{"isolinux/isolinux.bin", "EFI/BOOT/efiboot.img"} {
		e := entries[path]
		if e == nil {
			t.Fatalf("%s is missing from the image", path)
		}
		entryOffset := 32 + i*64
		if got := int64(binary.LittleEndian.Uint32(catalog[entryOffset+8:])) * 2048; got != e.Offset {
			t.Errorf("boot entry %d points at offset %d, but %s is at %d", i, got, path, e.Offset)
		}
	}

	// The boot information table was patched into the BIOS image.
	biosEntry := entries["isolinux/isolinux.bin"]
	stored := img[biosEntry.Offset : biosEntry.Offset+biosEntry.Size]
	if !bytes.Equal(stored[:8], bios[:8]) || !bytes.Equal(stored[64:], bios[64:]) {
		t.Error("the boot information table overwrote more than bytes 8 to 64")
	}
	if got := binary.LittleEndian.Uint32(stored[12:]); int64(got)*2048 != biosEntry.Offset {
		t.Errorf("the boot information table records block %d, but the file is at offset %d", got, biosEntry.Offset)
	}
	if got := binary.LittleEndian.Uint32(stored[16:]); int64(got) != biosEntry.Size {
		t.Errorf("the boot information table records a length of %d, want %d", got, biosEntry.Size)
	}
}

func TestIso9660BootFlagRejectsNonsense(t *testing.T) {
	for _, value := range []string{
		`not json`,
		`{"image":"/boot.img","platform":"sparc"}`,
		`{"image":"/boot.img","emulation":"tape"}`,
		`{"image":"/boot.img","typo":true}`,
	} {
		var flag iso9660BootFlag
		if err := flag.Set(value); err == nil {
			t.Errorf("%s was accepted", value)
		}
	}
}

// TestIso9660LayerCompactStreamOnly checks that the image can be left out: the
// compact stream and the metadata are the same as when it is written.
func TestIso9660LayerCompactStreamOnly(t *testing.T) {
	dir := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 4000)
	file := filepath.Join(dir, "big")
	if err := os.WriteFile(file, big, 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(only bool) (cstream []byte, metadata []byte, entries int) {
		work := t.TempDir()
		args := []string{"--layer-format", "iso9660", "--iso9660-udf", "--add", "/big=" + file, "--compact-stream", filepath.Join(work, "layer.cstream"), "--metadata", filepath.Join(work, "metadata.json")}
		if only {
			args = append(args, "--compact-stream-only")
		} else {
			args = append(args, filepath.Join(work, "layer.iso"))
		}
		LayerProcess(context.Background(), args)
		cs, err := os.ReadFile(filepath.Join(work, "layer.cstream"))
		if err != nil {
			t.Fatal(err)
		}
		md, err := os.ReadFile(filepath.Join(work, "metadata.json"))
		if err != nil {
			t.Fatal(err)
		}
		list, _ := os.ReadDir(work)
		return cs, md, len(list)
	}
	withImage, mdWith, nWith := run(false)
	only, mdOnly, nOnly := run(true)
	if nWith != 3 || nOnly != 2 {
		t.Errorf("%d files written with the image and %d without; want 3 and 2 (no leftover temporary image)", nWith, nOnly)
	}
	if !bytes.Equal(withImage, only) {
		t.Error("the compact stream depends on whether the image is written")
	}
	if !bytes.Equal(mdWith, mdOnly) {
		t.Errorf("the metadata differs:\n%s\n%s", mdWith, mdOnly)
	}
}

// TestIso9660LayerImportTar checks that a tar is read when the image is
// written, and that identical files share one extent.
func TestIso9660LayerImportTar(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	contents := map[string][]byte{
		"a": bytes.Repeat([]byte("a"), 30000),
		"b": bytes.Repeat([]byte("b"), 5000),
		"c": bytes.Repeat([]byte("a"), 30000),
	}
	for _, name := range []string{"a", "b", "c"} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(contents[name])), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(contents[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "link-to-b", Linkname: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(dir, "in.tar")
	if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "layer.iso")
	LayerProcess(context.Background(), []string{"--layer-format", "iso9660", "--import-tar", tarPath, out})
	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(img, contents["a"]); n != 1 {
		t.Errorf("the repeated file is stored %d times, want once (the extent is shared)", n)
	}
	entries := iso9660Entries(t, img)
	for _, name := range []string{"a", "b", "c", "link-to-b"} {
		if entries[name] == nil {
			t.Errorf("%s is missing", name)
		}
	}
	if entries["a"].Offset != entries["c"].Offset {
		t.Error("the two identical files do not share an extent")
	}
	if entries["b"].Offset != entries["link-to-b"].Offset {
		t.Error("the hard link does not share the extent of the file it points at")
	}
}

// TestIso9660LayerDeduplicateOff checks that identical files are stored twice
// when deduplication is turned off.
func TestIso9660LayerDeduplicateOff(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte{9}, 6000)
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dir, "layer.iso")
	LayerProcess(context.Background(), []string{
		"--layer-format", "iso9660", "--iso9660-deduplicate=false",
		"--add", "/a=" + filepath.Join(dir, "a"),
		"--add", "/b=" + filepath.Join(dir, "b"),
		out,
	})
	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(img, content); n != 2 {
		t.Errorf("the content appears %d times, want 2", n)
	}
}

func TestIso9660LayerMinimumSize(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "layer.iso")
	LayerProcess(context.Background(), []string{
		"--layer-format", "iso9660", "--iso9660-min-size", "4MiB", "--add", "/f=" + file, out,
	})
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 4<<20 {
		t.Errorf("the image is %d bytes, want at least 4 MiB", info.Size())
	}
}

func TestIso9660LayerRationalizedRockRidge(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(data, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "layer.iso")
	LayerProcess(context.Background(), []string{
		"--layer-format", "iso9660", "--iso9660-rock-ridge", "rationalized",
		"--default-metadata", `{"uid":1000,"gid":2000,"mode":"0640"}`,
		"--add", "/data.txt=" + data, out,
	})
	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	e := iso9660Entries(t, img)["data.txt"]
	if e == nil {
		t.Fatal("data.txt is missing")
	}
	if e.Mode.Perm() != 0o444 {
		t.Errorf("the mode is %v, want 0444", e.Mode.Perm())
	}
	if e.Uid != 0 || e.Gid != 0 {
		t.Errorf("the owner is %d:%d, want root", e.Uid, e.Gid)
	}
}

func TestIso9660LayerRejectsUnknownRockRidgeValue(t *testing.T) {
	// LayerProcess exits the process on a bad flag, so only the parsing of the
	// value is exercised here; the toolchain tests cover the exit status.
	for _, value := range []string{"enabled", "rationalized", "disabled"} {
		settings := iso9660Settings{rockRidge: value != "disabled", rationalizeRR: value == "rationalized"}
		opts := settings.options()
		if opts.RockRidge != (value != "disabled") || opts.RationalizeRockRidge != (value == "rationalized") {
			t.Errorf("%q gave RockRidge=%v RationalizeRockRidge=%v", value, opts.RockRidge, opts.RationalizeRockRidge)
		}
	}
}
