package disk

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// GUID is a globally unique identifier in its canonical (textual) byte order.
// On disk, GPT stores the first three fields little endian; see [GUID.onDisk].
type GUID [16]byte

// ParseGUID parses the canonical form "C12A7328-F81F-11D2-BA4B-00A0C93EC93B".
func ParseGUID(s string) (GUID, error) {
	var g GUID
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 || len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return g, fmt.Errorf("disk: %q is not a GUID like C12A7328-F81F-11D2-BA4B-00A0C93EC93B", s)
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return g, fmt.Errorf("disk: %q is not a GUID: %w", s, err)
	}
	copy(g[:], b)
	return g, nil
}

// String formats the GUID in upper case canonical form.
func (g GUID) String() string {
	h := strings.ToUpper(hex.EncodeToString(g[:]))
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// onDisk returns the 16 bytes GPT stores: the first three fields are little
// endian, the rest is as written.
func (g GUID) onDisk() [16]byte {
	b := g
	b[0], b[1], b[2], b[3] = g[3], g[2], g[1], g[0]
	b[4], b[5] = g[5], g[4]
	b[6], b[7] = g[7], g[6]
	return b
}

// derive returns a name-based (version 5 style) GUID from the digest of the
// parts. Equal inputs give equal GUIDs, which keeps disk images reproducible.
func derive(parts ...[]byte) GUID {
	h := sha256.New()
	for _, p := range parts {
		var n [8]byte
		for i := 0; i < 8; i++ {
			n[i] = byte(len(p) >> (8 * i))
		}
		h.Write(n[:])
		h.Write(p)
	}
	var g GUID
	copy(g[:], h.Sum(nil))
	g[6] = g[6]&0x0f | 0x50 // version 5
	g[8] = g[8]&0x3f | 0x80 // RFC 4122 variant
	return g
}

// TypeInfo describes a well-known partition type.
type TypeInfo struct {
	GUID    GUID
	MBRType byte
}

// Well-known partition types by name, from the UEFI specification and the
// Discoverable Partitions Specification.
var types = map[string]struct {
	guid    string
	mbrType byte
}{
	"esp":                  {"C12A7328-F81F-11D2-BA4B-00A0C93EC93B", 0xEF},
	"linux":                {"0FC63DAF-8483-4772-8E79-3D69D8477DE4", 0x83},
	"root-x86-64":          {"4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709", 0x83},
	"root-x86":             {"44479540-F297-41B2-9AF7-D131D5F0458A", 0x83},
	"root-arm64":           {"B921B045-1DF0-41C3-AF44-4C6F280D3FAE", 0x83},
	"root-arm":             {"69DAD710-2CE4-4E3C-B16C-21A1D49ABED3", 0x83},
	"root-riscv64":         {"72EC70A6-CF74-40E6-BD49-4BDA08E8F224", 0x83},
	"usr-x86-64":           {"8484680C-9521-48C6-9C11-B0720656F69E", 0x83},
	"usr-arm64":            {"B0E01050-EE5F-4390-949A-9101B17104E9", 0x83},
	"home":                 {"933AC7E1-2EB4-4F13-B844-0E14E2AEF915", 0x83},
	"srv":                  {"3B8F8425-20E0-4F3B-907F-1A25A76F98E8", 0x83},
	"var":                  {"4D21B016-B534-45C2-A9FB-5C16E091FD2D", 0x83},
	"tmp":                  {"7EC6F557-3BC5-4ACA-B293-16EF5DF639D1", 0x83},
	"swap":                 {"0657FD6D-A4AB-43C4-84E5-0933C84B4F4F", 0x82},
	"xbootldr":             {"BC13C2FF-59E6-4262-A352-B275FD6F7172", 0xEA},
	"bios-boot":            {"21686148-6449-6E6F-744E-656564454649", 0x83},
	"linux-lvm":            {"E6D6D379-F507-44C2-A23C-238F2A3DF928", 0x8E},
	"linux-raid":           {"A19D880F-05FC-4D3B-A006-743F0F84911E", 0xFD},
	"microsoft-basic-data": {"EBD0A0A2-B9E5-4433-87C0-68B6B72699C7", 0x07},
	"fat32":                {"EBD0A0A2-B9E5-4433-87C0-68B6B72699C7", 0x0C},
}

// LookupType resolves a partition type: one of the names of [TypeNames], or a
// GUID. The MBR type of a GUID that is not well known is 0x83 (Linux).
func LookupType(s string) (TypeInfo, error) {
	if t, ok := types[strings.ToLower(s)]; ok {
		g, err := ParseGUID(t.guid)
		if err != nil {
			return TypeInfo{}, err
		}
		return TypeInfo{GUID: g, MBRType: t.mbrType}, nil
	}
	g, err := ParseGUID(s)
	if err != nil {
		return TypeInfo{}, fmt.Errorf("disk: unknown partition type %q: not a name from %v and %w", s, TypeNames(), err)
	}
	return TypeInfo{GUID: g, MBRType: 0x83}, nil
}

// TypeNames lists the names of the well-known partition types, sorted.
func TypeNames() []string {
	names := make([]string, 0, len(types))
	for n := range types {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Partition attribute bits (GPT).
const (
	AttrRequired       uint64 = 1 << 0
	AttrNoBlockIO      uint64 = 1 << 1
	AttrLegacyBIOSBoot uint64 = 1 << 2
	AttrGrowFS         uint64 = 1 << 59
	AttrReadOnly       uint64 = 1 << 60
	AttrHidden         uint64 = 1 << 62
	AttrNoAutomount    uint64 = 1 << 63
	attributeNamesHelp        = "required, no_block_io, legacy_bios_bootable, growfs, read_only, hidden, no_automount or a number"
)

var attributeNames = map[string]uint64{
	"required":             AttrRequired,
	"no_block_io":          AttrNoBlockIO,
	"legacy_bios_bootable": AttrLegacyBIOSBoot,
	"growfs":               AttrGrowFS,
	"read_only":            AttrReadOnly,
	"hidden":               AttrHidden,
	"no_automount":         AttrNoAutomount,
}

// ParseAttribute resolves the name of an attribute bit, or a number, to a mask.
func ParseAttribute(s string) (uint64, error) {
	if v, ok := attributeNames[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.ParseUint(s, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("disk: unknown partition attribute %q: use %s", s, attributeNamesHelp)
	}
	return v, nil
}
