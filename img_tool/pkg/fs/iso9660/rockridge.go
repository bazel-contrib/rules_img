package iso9660

import (
	"fmt"
	"strings"
	"time"
)

// Rock Ridge (IEEE P1282) rides on the System Use Sharing Protocol (IEEE
// P1281): the unused tail of a directory record carries a sequence of entries,
// each a two-character signature, a length and a version. What does not fit in
// the record is moved into a continuation area, which a CE entry points at.

const (
	// ceEntryLength is the size of a CE entry, which has to stay in the record
	// itself.
	ceEntryLength = 28

	// rockRidgeExtVersion is the version of the extension the ER entry
	// declares.
	rockRidgeExtVersion = 1
)

// The identification an ER entry carries for Rock Ridge, as IEEE P1282
// prescribes it.
const (
	rrExtIdentifier  = "IEEE_P1282"
	rrExtDescriptor  = "THE IEEE P1282 PROTOCOL PROVIDES SUPPORT FOR POSIX FILE SYSTEM SEMANTICS."
	rrExtSource      = "PLEASE CONTACT THE IEEE STANDARDS DEPARTMENT, PISCATAWAY, NJ, USA FOR THE P1282 SPECIFICATION."
	rrMaxNamePayload = 250 // the bytes of a name one NM entry can hold
)

// suspEntry is one System Use entry, already encoded.
type suspEntry []byte

// newSUSPEntry starts an entry with the given signature and payload. The
// length byte counts the four bytes of the header as well.
func newSUSPEntry(sig string, version byte, payload []byte) suspEntry {
	e := make([]byte, 0, 4+len(payload))
	e = append(e, sig[0], sig[1], byte(4+len(payload)), version)
	return append(e, payload...)
}

// suspSP is the SUSP indicator, which has to be the first entry of the "."
// record of the root directory.
func suspSP() suspEntry {
	return newSUSPEntry("SP", 1, []byte{0xBE, 0xEF, 0x00})
}

// suspCE points at a continuation area: the block it starts in, the offset
// within that block and its length.
func suspCE(block, offset, length uint32) suspEntry {
	payload := make([]byte, 0, 24)
	payload = appendBoth32(payload, block)
	payload = appendBoth32(payload, offset)
	payload = appendBoth32(payload, length)
	return newSUSPEntry("CE", 1, payload)
}

// suspER identifies Rock Ridge as an extension that is in use. It only appears
// in the "." record of the root directory, and is far too large for the record
// itself, so it always lands in a continuation area.
func suspER() suspEntry {
	payload := []byte{
		byte(len(rrExtIdentifier)),
		byte(len(rrExtDescriptor)),
		byte(len(rrExtSource)),
		rockRidgeExtVersion,
	}
	payload = append(payload, rrExtIdentifier...)
	payload = append(payload, rrExtDescriptor...)
	payload = append(payload, rrExtSource...)
	return newSUSPEntry("ER", 1, payload)
}

// rripPX records the POSIX mode, link count and owner. The entry is the
// 36 byte form of RRIP 1.10, which also carries the only fields a reader uses:
// Linux identifies hard links by the extent they share, not by the inode
// number RRIP 1.12 added.
func rripPX(mode uint32, nlink int, uid, gid uint32) suspEntry {
	payload := make([]byte, 0, 32)
	payload = appendBoth32(payload, mode)
	payload = appendBoth32(payload, uint32(nlink))
	payload = appendBoth32(payload, uid)
	payload = appendBoth32(payload, gid)
	return newSUSPEntry("PX", 1, payload)
}

// rripPN records the device number of a character or block device, split into
// its high and low halves.
func rripPN(major, minor uint32) suspEntry {
	payload := make([]byte, 0, 16)
	payload = appendBoth32(payload, major)
	payload = appendBoth32(payload, minor)
	return newSUSPEntry("PN", 1, payload)
}

// Timestamps a TF entry may carry.
const (
	tfCreation   = 1 << 0
	tfModify     = 1 << 1
	tfAccess     = 1 << 2
	tfAttributes = 1 << 3
)

// rripTF records the modification, access and attribute change times in the
// seven byte form ISO 9660 uses in directory records.
func rripTF(mtime, atime, ctime time.Time) suspEntry {
	payload := make([]byte, 0, 1+3*7)
	payload = append(payload, tfModify|tfAccess|tfAttributes)
	payload = appendShortTime(payload, mtime)
	payload = appendShortTime(payload, atime)
	payload = appendShortTime(payload, ctime)
	return newSUSPEntry("TF", 1, payload)
}

// Flags of an NM entry.
const (
	nmContinue = 1 << 0
	nmCurrent  = 1 << 1
	nmParent   = 1 << 2
)

// rripNM records the real name of an entry, split over several entries if it
// does not fit in one.
func rripNM(name string) []suspEntry {
	var out []suspEntry
	for len(name) > rrMaxNamePayload {
		payload := append([]byte{nmContinue}, name[:rrMaxNamePayload]...)
		out = append(out, newSUSPEntry("NM", 1, payload))
		name = name[rrMaxNamePayload:]
	}
	return append(out, newSUSPEntry("NM", 1, append([]byte{0}, name...)))
}

// Flags of an SL entry and of the component records inside it.
const (
	slContinue = 1 << 0

	slCompContinue = 1 << 0
	slCompCurrent  = 1 << 1
	slCompParent   = 1 << 2
	slCompRoot     = 1 << 3
)

// rripSL records the target of a symbolic link as a list of component records,
// split over several entries if they do not fit in one. A component longer
// than a single record can hold is split as well, with the continue flag set.
func rripSL(target string) ([]suspEntry, error) {
	type component struct {
		flags byte
		text  string
	}
	var comps []component
	rest := target
	if strings.HasPrefix(rest, "/") {
		comps = append(comps, component{flags: slCompRoot})
		rest = strings.TrimPrefix(rest, "/")
	}
	for _, part := range strings.Split(rest, "/") {
		switch part {
		case "":
			// A trailing or doubled slash carries no component.
		case ".":
			comps = append(comps, component{flags: slCompCurrent})
		case "..":
			comps = append(comps, component{flags: slCompParent})
		default:
			comps = append(comps, component{text: part})
		}
	}
	if len(comps) == 0 {
		return nil, fmt.Errorf("symbolic link target %q has no components", target)
	}

	// Component records are packed into entries of at most 250 payload bytes
	// (255 minus the four header bytes and the flags byte).
	const maxPayload = 250
	var out []suspEntry
	payload := []byte{0}
	flush := func(more bool) {
		if more {
			payload[0] |= slContinue
		}
		out = append(out, newSUSPEntry("SL", 1, payload))
		payload = []byte{0}
	}
	for i, c := range comps {
		text := c.text
		for {
			room := maxPayload - len(payload) - 2
			if room < 1 && len(payload) > 1 {
				flush(true)
				continue
			}
			if room < 1 {
				return nil, fmt.Errorf("symbolic link target %q does not fit", target)
			}
			if len(text) <= room {
				payload = append(payload, c.flags, byte(len(text)))
				payload = append(payload, text...)
				break
			}
			payload = append(payload, c.flags|slCompContinue, byte(room))
			payload = append(payload, text[:room]...)
			text = text[room:]
			flush(true)
		}
		if i == len(comps)-1 {
			flush(false)
		}
	}
	return out, nil
}

// suspSize is the total length of a list of entries.
func suspSize(entries []suspEntry) int {
	total := 0
	for _, e := range entries {
		total += len(e)
	}
	return total
}

// splitSUSP divides the system use entries of a directory record into the part
// that stays in the record and the part that goes into a continuation area.
// The split keeps the order of the entries, so the SP entry stays in the
// record (where a reader looks for it) and the large ER entry moves out.
//
// avail is the space the record has left for the system use area. The caller
// has to reserve ceEntryLength of it for the CE entry when there is an
// overflow, which is what the second return value reports.
func splitSUSP(entries []suspEntry, avail int) (inline, overflow []suspEntry) {
	if suspSize(entries) <= avail {
		return entries, nil
	}
	budget := avail - ceEntryLength
	used := 0
	split := 0
	for i, e := range entries {
		if used+len(e) > budget {
			break
		}
		used += len(e)
		split = i + 1
	}
	return entries[:split], entries[split:]
}
