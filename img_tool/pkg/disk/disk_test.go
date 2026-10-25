package disk

import (
	"strings"
	"testing"
)

const (
	espGUID   = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	linuxGUID = "0FC63DAF-8483-4772-8E79-3D69D8477DE4"
	rootX64   = "4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709"
)

func mustPlan(t *testing.T, spec Spec) *Layout {
	t.Helper()
	l, err := Plan(spec)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestGPTLayout(t *testing.T) {
	l := mustPlan(t, Spec{
		Partitions: []Partition{
			{Label: "ESP", Type: "esp", ContentSize: 33_000_000},
			{Label: "placeholder", Type: "linux", Size: 5 << 20},
			{Label: "root", Type: "root-x86-64", ContentSize: 123_456, Attributes: AttrRequired | AttrNoAutomount},
			{Label: "data", Type: "linux", ContentSize: 1000, Size: 2 << 20}, // free space at the end
		},
	})
	disk := image(t, l)
	_, entries := checkGPT(t, disk, 512, 1<<20)
	if len(entries) != 4 {
		t.Fatalf("%d entries", len(entries))
	}

	// 33,000,000 bytes are 64,454 sectors, aligned up to 65,536 (32 MiB).
	want := []struct {
		name, typ    string
		start, count uint64
	}{
		{"ESP", espGUID, 2048, 65536 + 2048*0},
		{"placeholder", linuxGUID, 2048 + 65536, 5 << 11},
		{"root", rootX64, 2048 + 65536 + 5<<11, 2048},
		{"data", linuxGUID, 2048 + 65536 + 5<<11 + 2048, 2 << 11},
	}
	// 123,456 bytes are 242 sectors, aligned up to 2,048.
	for i, w := range want {
		e := entries[i]
		if e.name != w.name || e.typeGUID != w.typ || e.start != w.start || e.end-e.start+1 != w.count {
			t.Errorf("entry %d: %+v, want %+v", i, e, w)
		}
	}
	if entries[2].attrs != AttrRequired|AttrNoAutomount {
		t.Errorf("attributes %#x", entries[2].attrs)
	}
	for i, p := range l.Partitions {
		if p.StartLBA != entries[i].start || p.GUID.String() != entries[i].guid {
			t.Errorf("layout and table disagree on partition %d", i)
		}
	}

	// No free space at the end: the backup table follows the last partition.
	last := entries[3].end + 1
	if l.Sectors != last+32+1 {
		t.Errorf("disk has %d sectors, want %d (end of the last partition + 33)", l.Sectors, last+33)
	}
	if l.Size() != int64(len(disk)) {
		t.Errorf("size %d, image %d", l.Size(), len(disk))
	}
}

func TestSectorSize4096(t *testing.T) {
	l := mustPlan(t, Spec{SectorSize: 4096, Partitions: []Partition{
		{Label: "esp", Type: "esp", ContentSize: 300 << 20},
		{Label: "root", ContentSize: 5000},
	}})
	if l.FirstUsableLBA != 6 { // MBR, header and 4 sectors of entries
		t.Errorf("first usable LBA %d", l.FirstUsableLBA)
	}
	disk := image(t, l)
	_, entries := checkGPT(t, disk, 4096, 1<<20)
	// 1 MiB alignment is 256 sectors of 4096 bytes.
	if entries[0].start != 256 || entries[1].start != 256+300*256 {
		t.Errorf("starts %d and %d", entries[0].start, entries[1].start)
	}
	if l.Sectors != entries[1].end+1+4+1 {
		t.Errorf("disk has %d sectors", l.Sectors)
	}
}

func TestDiskSize(t *testing.T) {
	parts := []Partition{{Label: "a", ContentSize: 1 << 20}}
	base := mustPlan(t, Spec{Partitions: parts})

	if l := mustPlan(t, Spec{Partitions: parts, MinSize: 64 << 20}); l.Size() != 64<<20 {
		t.Errorf("min size: %d", l.Size())
	}
	if l := mustPlan(t, Spec{Partitions: parts, MinSize: 1 << 10}); l.Sectors != base.Sectors {
		t.Errorf("a small min size changed the disk: %d", l.Size())
	}
	if l := mustPlan(t, Spec{Partitions: parts, Size: 100 << 20}); l.Size() != 100<<20 {
		t.Errorf("exact size: %d", l.Size())
	} else {
		image := image(t, l)
		checkGPT(t, image, 512, 1<<20) // the backup table is at the end of the bigger disk
	}
	if l := mustPlan(t, Spec{Partitions: parts, MaxSize: base.Size()}); l.Sectors != base.Sectors {
		t.Errorf("max size equal to the need: %d", l.Size())
	}
	for name, spec := range map[string]Spec{
		"max too small":     {Partitions: parts, MaxSize: base.Size() - 512},
		"exact too small":   {Partitions: parts, Size: base.Size() - 512},
		"size with min":     {Partitions: parts, Size: 1 << 30, MinSize: 1 << 20},
		"min above max":     {Partitions: parts, MinSize: 2 << 20, MaxSize: 1 << 20},
		"negative size":     {Partitions: parts, Size: -1},
		"explicit too much": {Partitions: []Partition{{ContentSize: 2 << 20, Size: 1 << 20}}},
		"empty partition":   {Partitions: []Partition{{Label: "empty"}}},
	} {
		if _, err := Plan(spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDisklessAndSingle(t *testing.T) {
	// A disk without partitions is the tables only.
	l := mustPlan(t, Spec{})
	disk := image(t, l)
	_, entries := checkGPT(t, disk, 512, 1<<20)
	if len(entries) != 0 || l.Sectors != 34+1+33 {
		t.Errorf("%d entries, %d sectors", len(entries), l.Sectors)
	}
}

func TestAlignment(t *testing.T) {
	l := mustPlan(t, Spec{Alignment: 4096, Partitions: []Partition{{ContentSize: 100}, {ContentSize: 5000}}})
	checkGPT(t, image(t, l), 512, 4096)
	if l.Partitions[0].StartLBA != 40 || l.Partitions[0].Sectors != 8 || l.Partitions[1].StartLBA != 48 || l.Partitions[1].Sectors != 16 {
		t.Errorf("placement %+v", l.Partitions)
	}
	for _, a := range []int64{100, 513} {
		if _, err := Plan(Spec{Alignment: a}); err == nil {
			t.Errorf("alignment %d accepted", a)
		}
	}
}

func TestMBR(t *testing.T) {
	boot := []byte("boot code")
	l := mustPlan(t, Spec{Table: MBR, BootCode: boot, Partitions: []Partition{
		{Type: "esp", ContentSize: 40 << 20, Active: true},
		{Type: "linux", ContentSize: 1 << 20},
		{Type: "swap", Size: 8 << 20},
		{Type: "fat32", ContentSize: 5000},
	}})
	disk := image(t, l)
	if int64(len(disk)) != l.Size() || len(l.Head()) != 512 {
		t.Fatalf("disk %d bytes, head %d bytes", len(disk), len(l.Head()))
	}
	entries, sig := checkMBR(t, disk)
	if sig != l.DiskSignature || sig == 0 {
		t.Errorf("signature %#x", sig)
	}
	if string(disk[:len(boot)]) != "boot code" {
		t.Error("boot code missing")
	}
	if len(entries) != 4 {
		t.Fatalf("%d entries", len(entries))
	}
	for i, w := range []struct {
		active bool
		typ    byte
		start  uint32
		length uint32
	}{
		{true, 0xEF, 2048, 40 << 11}, {false, 0x83, 2048 + 40<<11, 2048}, {false, 0x82, 2048 + 40<<11 + 2048, 8 << 11}, {false, 0x0C, 2048 + 40<<11 + 2048 + 8<<11, 2048},
	} {
		e := entries[i]
		if e.active != w.active || e.typ != w.typ || e.start != w.start || e.len != w.length {
			t.Errorf("entry %d: %+v, want %+v", i, e, w)
		}
	}
	// No backup table: the disk ends with the last partition.
	if last := entries[3]; uint64(last.start+last.len) != l.Sectors {
		t.Errorf("disk has %d sectors, last partition ends at %d", l.Sectors, last.start+last.len)
	}
	if tail, _ := l.Tail(); tail != nil {
		t.Error("an MBR disk has a tail")
	}
	// LBA 2048 is cylinder 0, head 32, sector 33.
	if got := entries[0].chsStart; got != [3]byte{32, 33, 0} {
		t.Errorf("CHS of LBA 2048: % x", got)
	}
	// Beyond 1023 cylinders it is the maximum.
	if got := chs(32 << 20); got != [3]byte{0xFE, 0xFF, 0xFF} {
		t.Errorf("CHS beyond the limit: % x", got)
	}
}

func TestMBRErrors(t *testing.T) {
	five := make([]Partition, 5)
	for i := range five {
		five[i] = Partition{Size: 1 << 20}
	}
	two := []Partition{{Size: 1 << 20, Active: true}, {Size: 1 << 20, Active: true}}
	for name, spec := range map[string]Spec{
		"five partitions": {Table: MBR, Partitions: five},
		"two active":      {Table: MBR, Partitions: two},
		"too large":       {Table: MBR, Partitions: []Partition{{Size: 3 << 40}}},
		"active in gpt":   {Partitions: []Partition{{Size: 1 << 20, Active: true}}},
		"unknown table":   {Table: "bsd"},
		"sector size":     {SectorSize: 1024},
		"boot code":       {BootCode: make([]byte, 441)},
		"type":            {Partitions: []Partition{{Size: 1 << 20, Type: "ntfs-ish"}}},
		"label":           {Partitions: []Partition{{Size: 1 << 20, Label: strings.Repeat("x", 37)}}},
	} {
		if _, err := Plan(spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// 4096 byte sectors allow a larger MBR disk.
	if _, err := Plan(Spec{Table: MBR, SectorSize: 4096, Partitions: []Partition{{Size: 3 << 40}}}); err != nil {
		t.Errorf("3 TiB MBR disk with 4096 byte sectors: %v", err)
	}
	// 128 GPT partitions fit, 129 do not.
	many := make([]Partition, 129)
	for i := range many {
		many[i] = Partition{Size: 4096, Label: "p"}
	}
	if _, err := Plan(Spec{Alignment: 4096, Partitions: many[:128]}); err != nil {
		t.Error(err)
	}
	if _, err := Plan(Spec{Alignment: 4096, Partitions: many}); err == nil {
		t.Error("129 GPT partitions accepted")
	}
}

func TestGPTLimitsAndLabels(t *testing.T) {
	l := mustPlan(t, Spec{Alignment: 4096, Partitions: []Partition{{Label: strings.Repeat("ö", 35), Size: 4096}, {Label: "名前-😀", Size: 4096}}})
	_, entries := checkGPT(t, image(t, l), 512, 4096)
	if entries[0].name != strings.Repeat("ö", 35) || entries[1].name != "名前-😀" {
		t.Errorf("names %q %q", entries[0].name, entries[1].name)
	}
}

func TestDeterministicGUIDs(t *testing.T) {
	spec := Spec{Partitions: []Partition{{Label: "a", Size: 1 << 20}, {Label: "b", Size: 1 << 20}}}
	a, b := mustPlan(t, spec), mustPlan(t, spec)
	if string(a.Head()) != string(b.Head()) {
		t.Error("equal specs give different tables")
	}
	if a.DiskGUID == (GUID{}) || a.Partitions[0].GUID == a.Partitions[1].GUID || a.Partitions[0].GUID == a.DiskGUID {
		t.Errorf("GUIDs: %v %v %v", a.DiskGUID, a.Partitions[0].GUID, a.Partitions[1].GUID)
	}
	// RFC 4122 version 5 and variant.
	g := a.DiskGUID
	if g[6]>>4 != 5 || g[8]>>6 != 2 {
		t.Errorf("not a version 5 GUID: %v", g)
	}
	spec.Partitions[1].Label = "c"
	if c := mustPlan(t, spec); c.DiskGUID == a.DiskGUID {
		t.Error("a different spec gave the same disk GUID")
	}

	// Given GUIDs are used as they are.
	disk, _ := ParseGUID("11111111-2222-3333-4444-555555555555")
	part, _ := ParseGUID("AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE")
	l := mustPlan(t, Spec{DiskGUID: disk, Partitions: []Partition{{Size: 1 << 20, GUID: part}}})
	header, entries := checkGPT(t, image(t, l), 512, 1<<20)
	if canonical(header[56:72]) != "11111111-2222-3333-4444-555555555555" || entries[0].guid != "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE" {
		t.Errorf("GUIDs %s %s", canonical(header[56:72]), entries[0].guid)
	}
}

func TestGUIDs(t *testing.T) {
	g, err := ParseGUID(espGUID)
	if err != nil || g.String() != espGUID {
		t.Fatalf("%v %v", g, err)
	}
	// How the UEFI specification's example and sgdisk store the ESP type.
	want := []byte{0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11, 0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B}
	if got := g.onDisk(); string(got[:]) != string(want) {
		t.Errorf("on disk % x, want % x", got, want)
	}
	for _, bad := range []string{"", "C12A7328F81F11D2BA4B00A0C93EC93B", "C12A7328-F81F-11D2-BA4B-00A0C93EC93", "ZZZZZZZZ-F81F-11D2-BA4B-00A0C93EC93B"} {
		if _, err := ParseGUID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, name := range TypeNames() {
		ti, err := LookupType(name)
		if err != nil || ti.GUID == (GUID{}) {
			t.Errorf("type %q: %v", name, err)
		}
	}
	if ti, err := LookupType("ESP"); err != nil || ti.GUID.String() != espGUID || ti.MBRType != 0xEF {
		t.Errorf("ESP: %+v %v", ti, err)
	}
	if ti, err := LookupType("12345678-1234-1234-1234-123456789012"); err != nil || ti.MBRType != 0x83 {
		t.Errorf("custom GUID: %+v %v", ti, err)
	}
}

func TestParseSizeAndAttributes(t *testing.T) {
	for in, want := range map[string]int64{
		"0": 0, "4096": 4096, "1KiB": 1024, "64MiB": 64 << 20, "1.5GiB": 3 << 29, "2G": 2 << 30, "100MB": 100_000_000, "512 B": 512, " 3TiB ": 3 << 40,
		// The suffixes are case-insensitive, but KB is still decimal and K binary.
		"1kib": 1024, "1.5gib": 3 << 29, "64mIb": 64 << 20, "100mb": 100_000_000, "2g": 2 << 30, "8k": 8192, "8kb": 8000,
	} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "-1", "1.2.3", "MiB", "99999999999999TiB", "nan", "NaNGiB", "inf"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) accepted", bad)
		}
	}
	for in, want := range map[string]uint64{
		"required": 1, "NO_BLOCK_IO": 2, "legacy_bios_bootable": 4, "read_only": 1 << 60, "hidden": 1 << 62, "no_automount": 1 << 63, "growfs": 1 << 59, "0x10": 16, "7": 7,
	} {
		if got, err := ParseAttribute(in); err != nil || got != want {
			t.Errorf("ParseAttribute(%q) = %#x, %v; want %#x", in, got, err, want)
		}
	}
	if _, err := ParseAttribute("bogus"); err == nil {
		t.Error("bogus attribute accepted")
	}
}

func TestOffsets(t *testing.T) {
	l := mustPlan(t, Spec{Partitions: []Partition{{ContentSize: 1000}}})
	p := l.Partitions[0]
	if p.Offset(512) != 1<<20 || p.Length(512) != 1<<20 {
		t.Errorf("offset %d length %d", p.Offset(512), p.Length(512))
	}
}

func TestSizeBounds(t *testing.T) {
	// A size that is no multiple of the sector size never grows the disk.
	l := mustPlan(t, Spec{Size: 100<<20 + 1, Partitions: []Partition{{ContentSize: 100}}})
	if l.Sectors*512 != 100<<20 {
		t.Errorf("exact size gives %d bytes", l.Sectors*512)
	}
	// MinSize == MaxSize with a size no multiple of the sector size is satisfiable.
	l = mustPlan(t, Spec{MinSize: 100000001, MaxSize: 100000001, Partitions: []Partition{{ContentSize: 100}}})
	if l.Sectors != 100000001/512 {
		t.Errorf("%d sectors", l.Sectors)
	}
	if _, err := Plan(Spec{MaxSize: 1 << 20, Partitions: []Partition{{ContentSize: 2 << 20}}}); err == nil {
		t.Error("partitions larger than MaxSize accepted")
	}
}

func TestParseAttributeStrict(t *testing.T) {
	for _, s := range []string{"7garbage", "1 2", ""} {
		if _, err := ParseAttribute(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if v, err := ParseAttribute("0x10"); err != nil || v != 16 {
		t.Errorf("0x10: %d, %v", v, err)
	}
}

func TestLabelKeepsNUL(t *testing.T) {
	if _, err := Plan(Spec{Partitions: []Partition{{ContentSize: 1, Label: strings.Repeat("a", 36)}}}); err == nil {
		t.Error("36 character label accepted")
	}
	mustPlan(t, Spec{Partitions: []Partition{{ContentSize: 1, Label: strings.Repeat("a", 35)}}})
}

func TestAttributesChangeDerivedGUIDs(t *testing.T) {
	a := mustPlan(t, Spec{Partitions: []Partition{{ContentSize: 1}}})
	b := mustPlan(t, Spec{Partitions: []Partition{{ContentSize: 1, Attributes: AttrHidden}}})
	if a.DiskGUID == b.DiskGUID || a.Partitions[0].GUID == b.Partitions[0].GUID {
		t.Error("attributes do not change the derived GUIDs")
	}
}

func TestContentIDChangesDerivedGUIDs(t *testing.T) {
	plan := func(id string) *Layout {
		l, err := Plan(Spec{Partitions: []Partition{{Label: "root", Type: "linux", ContentSize: 1 << 20, ContentID: id}}})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	a, b, a2 := plan("a"), plan("b"), plan("a")
	if a.DiskGUID == b.DiskGUID || a.Partitions[0].GUID == b.Partitions[0].GUID {
		t.Error("different contents derived the same GUIDs")
	}
	if a.DiskGUID != a2.DiskGUID || a.Partitions[0].GUID != a2.Partitions[0].GUID {
		t.Error("the same contents derived different GUIDs")
	}
}
