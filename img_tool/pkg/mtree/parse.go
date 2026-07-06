package mtree

import (
	"fmt"
	"io"

	gomtree "github.com/vbatts/go-mtree"
)

// ParsedEntry is a single filesystem entry read back from an mtree spec: its path
// is decoded (vis-unescaped, canonicalized) and its keywords are collected into a
// map (keyword -> value, e.g. "type"->"file", "mode"->"0644", "uid"->"0",
// "sha256digest"->"<hex>"). It is the read counterpart of the WriteMulti renderer.
type ParsedEntry struct {
	// Path is the canonical entry path: no leading "./", no trailing "/". The
	// filesystem root is reported as ".".
	Path string
	// Keywords maps each mtree keyword to its (still vis-encoded, for paths/links)
	// value. Common keys: type, size, mode, uid, uname, gid, gname, sha256digest,
	// time, link, nlink, and xattr.<name>.
	Keywords map[string]string
}

// ParseEntries parses an mtree(8) specification (as produced by this package)
// into a flat list of entries with decoded paths and collected keywords. It uses
// the same path decoding and keyword handling as the renderer, so it round-trips
// WriteMulti output regardless of the operating system.
func ParseEntries(r io.Reader) ([]ParsedEntry, error) {
	dh, err := gomtree.ParseSpec(r)
	if err != nil {
		return nil, fmt.Errorf("parsing mtree: %w", err)
	}
	var out []ParsedEntry
	for i := range dh.Entries {
		e := dh.Entries[i]
		if e.Type != gomtree.FullType && e.Type != gomtree.RelativeType {
			continue // skip signature/comment/blank/special/dotdot lines
		}
		p, err := mtreeEntryPath(&e)
		if err != nil {
			return nil, fmt.Errorf("resolving mtree entry path: %w", err)
		}
		p = cleanPath(p)
		if p == "." {
			continue
		}
		keywords := map[string]string{}
		for _, keyval := range e.AllKeys() {
			keywords[string(keyval.Keyword())] = keyval.Value()
		}
		out = append(out, ParsedEntry{Path: p, Keywords: keywords})
	}
	return out, nil
}
