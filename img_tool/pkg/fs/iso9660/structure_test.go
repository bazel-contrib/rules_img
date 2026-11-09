package iso9660

import (
	"archive/tar"
	"strings"
	"testing"
)

// TestStructure runs the independent checker over a matrix of images, so every
// combination of the extensions is validated against the on-disk format rather
// than against the writer's own bookkeeping.
func TestStructure(t *testing.T) {
	populate := func(b *Builder) error {
		if err := b.WriteHeader(entryOf("etc/", tar.TypeDir, 0o755)); err != nil {
			return err
		}
		if err := b.WriteRegular(sized(entryOf("etc/Config File.conf", tar.TypeReg, 0o640), 5), strings.NewReader("value")); err != nil {
			return err
		}
		if err := b.WriteRegular(sized(entryOf("etc/nested/deeply/placed/file.txt", tar.TypeReg, 0o644), 4), strings.NewReader("deep")); err != nil {
			return err
		}
		// Enough children that the directory needs more than one block.
		for i := 0; i < 120; i++ {
			name := "many/" + strings.Repeat("x", 40) + string(rune('a'+i%26)) + string(rune('a'+i/26))
			if err := b.WriteRegular(sized(entryOf(name, tar.TypeReg, 0o644), 1), strings.NewReader("x")); err != nil {
				return err
			}
		}
		return b.WriteRegular(sized(entryOf("empty", tar.TypeReg, 0o644), 0), strings.NewReader(""))
	}
	withRockRidge := func(b *Builder) error {
		if err := populate(b); err != nil {
			return err
		}
		link := entryOf("etc/link", tar.TypeSymlink, 0o777)
		link.Linkname = "/etc/Config File.conf"
		if err := b.WriteHeader(link); err != nil {
			return err
		}
		dev := entryOf("dev/null", tar.TypeChar, 0o666)
		dev.Devmajor, dev.Devminor = 1, 3
		if err := b.WriteHeader(dev); err != nil {
			return err
		}
		if err := b.WriteHeader(entryOf("dev/pipe", tar.TypeFifo, 0o644)); err != nil {
			return err
		}
		hard := entryOf("etc/also-config", tar.TypeLink, 0o640)
		hard.Linkname = "etc/Config File.conf"
		return b.WriteHeader(hard)
	}

	for _, tc := range []struct {
		name string
		opts Options
		add  func(*Builder) error
	}{
		{"plain", Options{}, populate},
		{"rock_ridge", Options{RockRidge: true}, withRockRidge},
		{"joliet", Options{Joliet: true}, populate},
		{"joliet_long", Options{Joliet: true, JolietLongNames: true}, populate},
		{"udf", Options{UDF: true}, populate},
		{"rock_ridge_joliet", Options{RockRidge: true, Joliet: true}, withRockRidge},
		{"everything", Options{RockRidge: true, Joliet: true, UDF: true, Deduplicate: true}, withRockRidge},
		{"empty", Options{RockRidge: true, Joliet: true, UDF: true}, func(*Builder) error { return nil }},
		{"padded", Options{UDF: true, Size: 8 << 20}, populate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, _ := build(t, tc.opts, tc.add)
			c := check(t, img)
			if c.rockRidge != tc.opts.RockRidge {
				t.Errorf("the image announces Rock Ridge = %v, want %v", c.rockRidge, tc.opts.RockRidge)
			}
		})
	}
}

// TestStructureOfABootableImage checks an image with El Torito entries for
// both firmwares, which is how a hybrid installer image is laid out.
func TestStructureOfABootableImage(t *testing.T) {
	const biosImage = "isolinux/isolinux.bin"
	const efiImage = "EFI/BOOT/efiboot.img"
	opts := Options{
		RockRidge: true,
		Joliet:    true,
		UDF:       true,
		Boot: []BootEntry{
			{Image: biosImage, Platform: BootPlatformX86, LoadSize: 4, BootInfoTable: true},
			{Image: efiImage, Platform: BootPlatformEFI, ID: "EFI"},
		},
	}
	img, info := build(t, opts, func(b *Builder) error {
		if err := b.WriteRegular(sized(entryOf(biosImage, tar.TypeReg, 0o644), 4096), strings.NewReader(strings.Repeat("B", 4096))); err != nil {
			return err
		}
		return b.WriteRegular(sized(entryOf(efiImage, tar.TypeReg, 0o644), 2048), strings.NewReader(strings.Repeat("E", 2048)))
	})
	c := check(t, img)

	if info.BootCatalog == 0 {
		t.Error("Info.BootCatalog is zero for a bootable image")
	}
	entries := walk(t, img)
	lbas := c.bootImageLBAs()
	if len(lbas) != 2 {
		t.Fatalf("the catalog has %d boot entries, want 2", len(lbas))
	}
	for i, path := range []string{biosImage, efiImage} {
		e := entries[path]
		if e == nil {
			t.Fatalf("%s is missing from the image", path)
		}
		if int64(lbas[i])*BlockSize != e.Offset {
			t.Errorf("boot entry %d points at block %d, but %s is at offset %d", i, lbas[i], path, e.Offset)
		}
	}

	// The boot information table was patched into the BIOS image, so its
	// stored content is not the file that went in and it carries no digest.
	bios := entries[biosImage]
	data := []byte(readContent(t, mustOpen(t, img), bios))
	if string(data[:8]) != strings.Repeat("B", 8) {
		t.Error("the bytes before the boot information table were overwritten")
	}
	if le32(data[12:]) != lbas[0] {
		t.Errorf("the boot information table records block %d, want %d", le32(data[12:]), lbas[0])
	}
	if le32(data[16:]) != 4096 {
		t.Errorf("the boot information table records a length of %d, want 4096", le32(data[16:]))
	}
	if string(data[64:72]) != strings.Repeat("B", 8) {
		t.Error("the bytes after the boot information table were overwritten")
	}
}

func TestBootEntryErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		boot []BootEntry
		add  func(*Builder) error
		want string
	}{
		{
			name: "missing image",
			boot: []BootEntry{{Image: "boot/absent.img"}},
			add:  func(*Builder) error { return nil },
			want: "not in the image",
		},
		{
			name: "directory",
			boot: []BootEntry{{Image: "boot"}},
			add:  func(b *Builder) error { return b.WriteHeader(entryOf("boot/", tar.TypeDir, 0o755)) },
			want: "not a regular file",
		},
		{
			name: "too small for a boot information table",
			boot: []BootEntry{{Image: "boot.img", BootInfoTable: true}},
			add: func(b *Builder) error {
				return b.WriteRegular(sized(entryOf("boot.img", tar.TypeReg, 0o644), 8), strings.NewReader("12345678"))
			},
			want: "too small for a boot information table",
		},
		{
			name: "patched image is shared",
			boot: []BootEntry{{Image: "boot.img", BootInfoTable: true}},
			add: func(b *Builder) error {
				content := strings.Repeat("x", 1024)
				if err := b.WriteRegular(sized(entryOf("boot.img", tar.TypeReg, 0o644), 1024), strings.NewReader(content)); err != nil {
					return err
				}
				link := entryOf("other.img", tar.TypeLink, 0o644)
				link.Linkname = "boot.img"
				return b.WriteHeader(link)
			},
			want: "shares its content",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := NewBuilder(Options{RockRidge: true, Time: epoch, Boot: tc.boot})
			if err != nil {
				t.Fatalf("NewBuilder: %v", err)
			}
			defer b.Close()
			if err := tc.add(b); err != nil {
				t.Fatalf("adding entries: %v", err)
			}
			_, err = b.Build(&memImage{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Build gave %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
