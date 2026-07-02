// Package mtree renders an mtree(8) specification describing the metadata of one
// or more container image layers.
//
// Inputs are processed in order and may be interleaved (see WriteMulti / Input):
//
//   - Tar inputs (a possibly-compressed layer blob, or a compact stream
//     reconstructed with zero-filled bodies) are read as tar streams; passing
//     several is equivalent to concatenating them into one stream. Each regular
//     file's sha256digest is supplied by a ContentDigester (hashed content for a
//     materialized tar; the recorded CAS reference digest or inlined bytes for a
//     compact stream). Hardlinks (tar TypeLink) are replaced by a copy of the
//     entry they point at, so "link" is emitted only for symlinks.
//   - Mtree inputs are existing mtree specs; their entries are folded in as
//     though they came from a tar. Their paths are re-normalized to the
//     configured layout and their keywords are filtered down to the requested
//     option set (e.g. a stray nlink is stripped if nlink was not requested).
//
// Two layouts are supported (Options.Layout):
//
//   - "tar" emits one entry per input entry, in order, keeping whiteout markers
//     verbatim and never synthesizing intermediate directories.
//   - "oci_layer_filesystem_applied_changeset" applies every input (tar and
//     mtree) to an in-memory filesystem as an OCI changeset -- synthesizing
//     missing parent directories and applying whiteout (".wh.<name>") and
//     opaque-whiteout (".wh..wh..opq") markers -- and serializes the resulting
//     tree in a stable, path-sorted order.
//
// Output is deterministic and host-independent: header keyword values come
// straight from the tar header (never the host filesystem or an /etc/passwd
// lookup), extended attributes are emitted in sorted order, and the entry order
// is fixed by the layout. The mtree data model, escaping (govis), spec parsing,
// and text rendering all come from github.com/vbatts/go-mtree; only the per-entry
// keyword selection is done here, so the result stays byte-stable across
// operating systems (unlike go-mtree's own tar streamer).
package mtree

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
	gomtree "github.com/vbatts/go-mtree"
	"github.com/vbatts/go-mtree/pkg/govis"
)

// Layout values for Options.Layout.
const (
	// LayoutTar emits one mtree entry per input entry, in input order.
	LayoutTar = "tar"
	// LayoutOCIChangeset applies the inputs as an OCI changeset to an empty
	// filesystem and serializes the resulting tree in a stable order.
	LayoutOCIChangeset = "oci_layer_filesystem_applied_changeset"
)

// schilyXattrPrefix is the PAX record namespace under which tar stores extended
// attributes. Records with this prefix are rendered as mtree `xattr.<name>`
// keywords when the "xattr" field is requested.
const schilyXattrPrefix = "SCHILY.xattr."

// OCI layer whiteout markers (see the OCI image-spec layer changeset rules).
const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// Options controls how the inputs are rendered as an mtree spec.
type Options struct {
	// PathPrefix is prepended to every entry path. It must be "" or "./". With
	// "" (bare tar paths), directory entries get a trailing "/" so they remain
	// full-path (not relative) entries.
	PathPrefix string
	// Keywords is the ordered list of fields to emit, on a best-effort basis.
	// Supported: type, size, mode, uid, uname, gid, gname, sha256, time, link,
	// nlink, xattr. Unknown names are ignored.
	Keywords []string
	// Layout is one of LayoutTar or LayoutOCIChangeset.
	Layout string
}

// DefaultOptions returns the default mtree rendering options.
func DefaultOptions() Options {
	return Options{
		PathPrefix: "./",
		Keywords:   []string{"type", "size", "mode", "uid", "uname", "gid", "gname", "sha256", "time", "link", "nlink"},
		Layout:     LayoutTar,
	}
}

// InputKind identifies how an Input's Reader is interpreted.
type InputKind int

const (
	// TarInput reads the Reader as an uncompressed tar stream.
	TarInput InputKind = iota
	// MtreeInput reads the Reader as an existing mtree spec.
	MtreeInput
)

// Input is one ordered source of entries.
type Input struct {
	Kind InputKind
	// Reader is an uncompressed tar (TarInput) or an mtree spec (MtreeInput).
	Reader io.Reader
	// Digester supplies content digests for TarInput regular files. Ignored for
	// MtreeInput.
	Digester ContentDigester
}

// ContentDigester computes the sha256 of a regular file's content for the
// sha256digest keyword. It is called once per regular file with a non-zero size,
// with the tar reader positioned at that file's content. An implementation must
// EITHER read exactly the file's content from `content` (e.g. to hash inline
// bytes) OR return a precomputed digest without reading from `content` (leaving
// the body for the tar reader to skip). Returning a nil digest omits the
// sha256digest keyword for that entry.
type ContentDigester func(hdr *tar.Header, content io.Reader) ([]byte, error)

// HashContent is the default ContentDigester: it reads the whole content and
// returns its sha256. Use it when the real file bytes are available (e.g. a
// materialized tar blob).
func HashContent(_ *tar.Header, content io.Reader) ([]byte, error) {
	h := sha256.New()
	if _, err := io.Copy(h, content); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// WriteFromUncompressedTar reads an uncompressed tar from r and writes a
// deterministic mtree specification (with DefaultOptions and content digests
// hashed from each file) to w.
func WriteFromUncompressedTar(r io.Reader, w io.Writer) error {
	return Write(r, w, DefaultOptions(), HashContent)
}

// Write renders a single uncompressed tar input. It is a convenience wrapper
// around WriteMulti.
func Write(r io.Reader, w io.Writer, opts Options, digester ContentDigester) error {
	return WriteMulti(w, opts, []Input{{Kind: TarInput, Reader: r, Digester: digester}})
}

// WriteMulti renders the ordered inputs to a single mtree spec on w.
func WriteMulti(w io.Writer, opts Options, inputs []Input) error {
	var collected []collectedEntry
	for _, in := range inputs {
		switch in.Kind {
		case TarInput:
			es, err := collectTar(in.Reader, in.Digester)
			if err != nil {
				return err
			}
			collected = append(collected, es...)
		case MtreeInput:
			es, err := collectMtree(in.Reader, opts)
			if err != nil {
				return err
			}
			collected = append(collected, es...)
		default:
			return fmt.Errorf("unknown mtree input kind %d", in.Kind)
		}
	}

	inodes, hardlinkOf := groupHardlinks(collected)

	dh := &gomtree.DirectoryHierarchy{}
	appendEntry(dh, gomtree.Entry{Type: gomtree.SignatureType, Raw: "#mtree v2.0"})

	switch opts.Layout {
	case "", LayoutTar:
		for i := range collected {
			p, isDir, kvs, err := materialize(collected, inodes, hardlinkOf, i, opts)
			if err != nil {
				return err
			}
			entry, err := buildEntry(p, isDir, kvs, opts)
			if err != nil {
				return err
			}
			appendEntry(dh, entry)
		}
	case LayoutOCIChangeset:
		if err := writeChangeset(dh, collected, inodes, hardlinkOf, opts); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown mtree layout %q", opts.Layout)
	}

	if _, err := dh.WriteTo(w); err != nil {
		return fmt.Errorf("writing mtree: %w", err)
	}
	return nil
}

// collectedEntry is one entry gathered from an input, in input order. Tar-origin
// entries carry a tar header (+ digest) to be rendered later; mtree-origin
// entries carry the already-filtered keywords parsed from the source spec.
type collectedEntry struct {
	path    string           // canonical path (no prefix, no trailing slash)
	hdr     *tar.Header      // tar-origin; nil for mtree-origin
	digest  []byte           // tar-origin content sha256
	keyvals []gomtree.KeyVal // mtree-origin, already filtered/ordered
	isDir   bool             // mtree-origin (tar-origin derives it from hdr)
}

// inode holds the metadata shared by a regular file and any hardlinks to it.
type inode struct {
	hdr    *tar.Header
	digest []byte
	nlink  int
}

// collectTar reads an uncompressed tar and captures every entry with its content
// digest (obtained from digester). Hardlink grouping is done globally afterwards.
func collectTar(r io.Reader, digester ContentDigester) ([]collectedEntry, error) {
	tr := tar.NewReader(r)
	var out []collectedEntry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar: %w", err)
		}
		var digest []byte
		if digester != nil && isRegularFile(hdr) && hdr.Size > 0 {
			digest, err = digester(hdr, tr)
			if err != nil {
				return nil, fmt.Errorf("digesting %q: %w", hdr.Name, err)
			}
		}
		out = append(out, collectedEntry{path: cleanPath(hdr.Name), hdr: hdr, digest: digest})
	}
	return out, nil
}

// collectMtree parses an existing mtree spec and folds its entries in, filtering
// each entry's keywords down to the requested options and canonicalizing paths.
func collectMtree(r io.Reader, opts Options) ([]collectedEntry, error) {
	dh, err := gomtree.ParseSpec(r)
	if err != nil {
		return nil, fmt.Errorf("parsing mtree input: %w", err)
	}
	var out []collectedEntry
	for i := range dh.Entries {
		e := dh.Entries[i]
		if e.Type != gomtree.FullType && e.Type != gomtree.RelativeType {
			continue // skip signature/comment/blank/special/dotdot lines
		}
		p, err := e.Path()
		if err != nil {
			return nil, fmt.Errorf("resolving mtree entry path: %w", err)
		}
		p = cleanPath(p)
		if p == "." {
			continue
		}
		all := e.AllKeys()
		out = append(out, collectedEntry{
			path:    p,
			keyvals: filterKeyvals(all, opts.Keywords),
			isDir:   hasTypeDir(all),
		})
	}
	return out, nil
}

// groupHardlinks builds the inode map over all tar-origin regular files (across
// every tar input, as if concatenated) and points each tar hardlink at its
// target's inode, counting nlink.
func groupHardlinks(collected []collectedEntry) (map[string]*inode, map[int]*inode) {
	inodes := map[string]*inode{}
	for i := range collected {
		ce := &collected[i]
		if ce.hdr != nil && isRegularFile(ce.hdr) {
			inodes[ce.path] = &inode{hdr: ce.hdr, digest: ce.digest, nlink: 1}
		}
	}
	hardlinkOf := map[int]*inode{}
	for i := range collected {
		ce := &collected[i]
		if ce.hdr != nil && ce.hdr.Typeflag == tar.TypeLink {
			target := cleanPath(ce.hdr.Linkname)
			if in, ok := inodes[target]; ok {
				in.nlink++
				hardlinkOf[i] = in
			} else {
				hardlinkOf[i] = &inode{hdr: ce.hdr, nlink: 1}
			}
		}
	}
	return inodes, hardlinkOf
}

// materialize computes the final (path, isDir, keywords) for collected entry i.
func materialize(collected []collectedEntry, inodes map[string]*inode, hardlinkOf map[int]*inode, i int, opts Options) (string, bool, []gomtree.KeyVal, error) {
	ce := &collected[i]
	if ce.hdr == nil { // mtree-origin: already filtered
		return ce.path, ce.isDir, ce.keyvals, nil
	}
	if ce.hdr.Typeflag == tar.TypeLink { // hardlink: render as a copy of the target
		in := hardlinkOf[i]
		kvs, err := keywordsForHeader(in.hdr, in.digest, in.nlink, opts.Keywords)
		return ce.path, false, kvs, err
	}
	nlink := 1
	if in, ok := inodes[ce.path]; ok {
		nlink = in.nlink
	}
	kvs, err := keywordsForHeader(ce.hdr, ce.digest, nlink, opts.Keywords)
	return ce.path, ce.hdr.Typeflag == tar.TypeDir, kvs, err
}

// treeNode is a node in the in-memory filesystem tree for the changeset layout.
// A synthesized intermediate directory has synthesized=true and no keywords.
type treeNode struct {
	name        string
	keyvals     []gomtree.KeyVal
	isDir       bool
	synthesized bool
	children    map[string]*treeNode
}

func newDir() *treeNode {
	return &treeNode{isDir: true, synthesized: true, children: map[string]*treeNode{}}
}

// ensureDir walks/creates the directory path components under root, synthesizing
// missing intermediate directories, and returns the deepest one.
func (root *treeNode) ensureDir(components []string) *treeNode {
	cur := root
	for _, comp := range components {
		child := cur.children[comp]
		if child == nil {
			child = &treeNode{name: comp, isDir: true, synthesized: true, children: map[string]*treeNode{}}
			cur.children[comp] = child
		} else if !child.isDir {
			// A non-directory exists where a directory is needed; convert it
			// (best effort for malformed inputs).
			child.isDir = true
			child.synthesized = true
			child.keyvals = nil
			child.children = map[string]*treeNode{}
		}
		cur = child
	}
	return cur
}

// writeChangeset applies the collected entries as an OCI changeset to an empty
// tree and serializes it in a stable, path-sorted order.
func writeChangeset(dh *gomtree.DirectoryHierarchy, collected []collectedEntry, inodes map[string]*inode, hardlinkOf map[int]*inode, opts Options) error {
	root := newDir()
	for i := range collected {
		p, isDir, kvs, err := materialize(collected, inodes, hardlinkOf, i, opts)
		if err != nil {
			return err
		}
		if p == "." {
			continue
		}
		comps := strings.Split(p, "/")
		base := comps[len(comps)-1]
		parentComps := comps[:len(comps)-1]

		switch {
		case base == whiteoutOpaque:
			// Opaque whiteout: nothing below an empty base to hide, so consume it.
			root.ensureDir(parentComps)
			continue
		case strings.HasPrefix(base, whiteoutPrefix):
			// Regular whiteout: remove the named sibling if present.
			parent := root.ensureDir(parentComps)
			delete(parent.children, strings.TrimPrefix(base, whiteoutPrefix))
			continue
		}

		parent := root.ensureDir(parentComps)
		node := parent.children[base]
		if node == nil {
			node = &treeNode{name: base}
			parent.children[base] = node
		}
		node.keyvals = kvs
		node.isDir = isDir
		node.synthesized = false
		if isDir && node.children == nil {
			node.children = map[string]*treeNode{}
		}
	}

	type named struct {
		path string
		node *treeNode
	}
	var all []named
	var collectNodes func(prefix string, n *treeNode)
	collectNodes = func(prefix string, n *treeNode) {
		for name, child := range n.children {
			p := name
			if prefix != "" {
				p = prefix + "/" + name
			}
			all = append(all, named{p, child})
			if child.isDir {
				collectNodes(p, child)
			}
		}
	}
	collectNodes("", root)
	sort.Slice(all, func(i, j int) bool { return all[i].path < all[j].path })

	for _, nn := range all {
		kvs := nn.node.keyvals
		if nn.node.synthesized {
			kvs = []gomtree.KeyVal{gomtree.KeyVal("type=dir")}
		}
		entry, err := buildEntry(nn.path, nn.node.isDir, kvs, opts)
		if err != nil {
			return err
		}
		appendEntry(dh, entry)
	}
	return nil
}

// appendEntry adds e to the hierarchy, assigning it the next position so
// DirectoryHierarchy.WriteTo emits entries in insertion order.
func appendEntry(dh *gomtree.DirectoryHierarchy, e gomtree.Entry) {
	e.Pos = len(dh.Entries)
	dh.Entries = append(dh.Entries, e)
}

// buildEntry builds a FullType mtree entry: the path is govis-encoded and given
// the configured prefix, with a trailing slash added to directories only when
// the prefix is empty (so they still contain a slash and parse as full paths).
func buildEntry(p string, isDir bool, kvs []gomtree.KeyVal, opts Options) (gomtree.Entry, error) {
	encoded, err := govis.Vis(p, gomtree.DefaultVisFlags)
	if err != nil {
		return gomtree.Entry{}, err
	}
	name := opts.PathPrefix + encoded
	if isDir && opts.PathPrefix == "" {
		name += "/"
	}
	return gomtree.Entry{Name: name, Keywords: kvs, Type: gomtree.FullType}, nil
}

// keywordsForHeader computes the requested keywords for a tar header, in the
// order given by keywords, on a best-effort basis. Values are derived solely
// from the header, so the result is byte-stable across hosts.
func keywordsForHeader(hdr *tar.Header, digest []byte, nlink int, keywords []string) ([]gomtree.KeyVal, error) {
	typ := mtreeType(hdr)
	mode := hdr.FileInfo().Mode()
	var kvs []gomtree.KeyVal
	for _, kw := range keywords {
		switch kw {
		case "type":
			kvs = append(kvs, gomtree.KeyVal("type="+typ))
		case "size":
			if typ != "dir" {
				if hdr.Typeflag == tar.TypeSymlink {
					kvs = append(kvs, kv("size=%d", len(hdr.Linkname)))
				} else {
					kvs = append(kvs, kv("size=%d", hdr.Size))
				}
			}
		case "mode":
			perm := mode.Perm()
			if mode&os.ModeSetuid != 0 {
				perm |= 1 << 11
			}
			if mode&os.ModeSetgid != 0 {
				perm |= 1 << 10
			}
			if mode&os.ModeSticky != 0 {
				perm |= 1 << 9
			}
			kvs = append(kvs, kv("mode=%#o", perm))
		case "uid":
			kvs = append(kvs, kv("uid=%d", hdr.Uid))
		case "gid":
			kvs = append(kvs, kv("gid=%d", hdr.Gid))
		case "uname":
			if hdr.Uname != "" {
				kvs = append(kvs, gomtree.KeyVal("uname="+hdr.Uname))
			}
		case "gname":
			if hdr.Gname != "" {
				kvs = append(kvs, gomtree.KeyVal("gname="+hdr.Gname))
			}
		case "sha256":
			if len(digest) > 0 {
				kvs = append(kvs, kv("sha256digest=%x", digest))
			}
		case "time":
			kvs = append(kvs, kv("time=%d", hdr.ModTime.Unix()))
		case "link":
			if hdr.Typeflag == tar.TypeSymlink && hdr.Linkname != "" {
				encoded, err := govis.Vis(hdr.Linkname, gomtree.DefaultVisFlags)
				if err != nil {
					return nil, err
				}
				kvs = append(kvs, gomtree.KeyVal("link="+encoded))
			}
		case "nlink":
			kvs = append(kvs, kv("nlink=%d", nlink))
		case "xattr":
			xattrs, err := xattrKeywords(hdr)
			if err != nil {
				return nil, err
			}
			kvs = append(kvs, xattrs...)
		}
	}
	return kvs, nil
}

// filterKeyvals keeps only the parsed keyvals matching the requested options, in
// the requested option order. This strips any keyword an mtree input carried
// that the caller did not ask for (e.g. nlink).
func filterKeyvals(parsed []gomtree.KeyVal, options []string) []gomtree.KeyVal {
	var out []gomtree.KeyVal
	for _, opt := range options {
		if opt == "xattr" {
			var xs []gomtree.KeyVal
			for _, kv := range parsed {
				if kv.Keyword().Prefix() == "xattr" {
					xs = append(xs, kv)
				}
			}
			sort.Slice(xs, func(i, j int) bool { return xs[i].Keyword() < xs[j].Keyword() })
			out = append(out, xs...)
			continue
		}
		want := optionKeyword(opt)
		for _, kv := range parsed {
			if string(kv.Keyword()) == want {
				out = append(out, kv)
			}
		}
	}
	return out
}

// optionKeyword maps an option name to the mtree keyword it selects.
func optionKeyword(opt string) string {
	if opt == "sha256" {
		return "sha256digest"
	}
	return opt
}

// hasTypeDir reports whether a parsed keyword set marks the entry as a directory.
func hasTypeDir(kvs []gomtree.KeyVal) bool {
	for _, kv := range kvs {
		if kv.Keyword() == "type" {
			return kv.Value() == "dir"
		}
	}
	return false
}

// mtreeType maps a tar header to an mtree type= value.
func mtreeType(hdr *tar.Header) string {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return "dir"
	case tar.TypeSymlink:
		return "link"
	case tar.TypeChar:
		return "char"
	case tar.TypeBlock:
		return "block"
	case tar.TypeFifo:
		return "fifo"
	case tar.TypeReg, tar.TypeRegA, tar.TypeLink, tar.TypeCont:
		return "file"
	}
	switch m := hdr.FileInfo().Mode(); {
	case m.IsDir():
		return "dir"
	case m&os.ModeSymlink != 0:
		return "link"
	case m&os.ModeNamedPipe != 0:
		return "fifo"
	case m&os.ModeDevice != 0:
		if m&os.ModeCharDevice != 0 {
			return "char"
		}
		return "block"
	default:
		return "file"
	}
}

// xattrKeywords renders SCHILY.xattr.* PAX records as `xattr.<name>=<base64>`
// keywords, sorted by attribute name for a stable order.
func xattrKeywords(hdr *tar.Header) ([]gomtree.KeyVal, error) {
	if len(hdr.PAXRecords) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(hdr.PAXRecords))
	for k := range hdr.PAXRecords {
		if strings.HasPrefix(k, schilyXattrPrefix) {
			names = append(names, strings.TrimPrefix(k, schilyXattrPrefix))
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	sort.Strings(names)
	kvs := make([]gomtree.KeyVal, 0, len(names))
	for _, name := range names {
		encKey, err := govis.Vis(name, gomtree.DefaultVisFlags)
		if err != nil {
			return nil, err
		}
		value := hdr.PAXRecords[schilyXattrPrefix+name]
		kvs = append(kvs, gomtree.KeyVal(fmt.Sprintf("xattr.%s=%s", encKey, base64.StdEncoding.EncodeToString([]byte(value)))))
	}
	return kvs, nil
}

// cleanPath normalizes a tar/mtree entry path to its canonical form (no trailing
// slash, no leading "./"), mapping the degenerate empty/root name to ".".
func cleanPath(name string) string {
	n := path.Clean(strings.TrimSuffix(name, "/"))
	if n == "" || n == "." {
		return "."
	}
	return n
}

// isRegularFile reports whether the header describes a regular file (the only
// entry type that carries content to digest and the only valid hardlink target).
func isRegularFile(hdr *tar.Header) bool {
	return hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA
}

func kv(format string, args ...any) gomtree.KeyVal {
	return gomtree.KeyVal(fmt.Sprintf(format, args...))
}

// Decompress returns a reader over the uncompressed tar bytes of r, sniffing the
// gzip and zstd magic numbers (and otherwise treating the input as a plain tar).
// Compression is detected by content, not file extension.
func Decompress(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(4)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("peeking compression magic: %w", err)
	}
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("creating gzip reader: %w", err)
		}
		return gz, nil
	case len(magic) >= 4 && magic[0] == 0x28 && magic[1] == 0xb5 && magic[2] == 0x2f && magic[3] == 0xfd:
		zr, err := zstd.NewReader(br)
		if err != nil {
			return nil, fmt.Errorf("creating zstd reader: %w", err)
		}
		return zr.IOReadCloser(), nil
	default:
		return br, nil
	}
}
