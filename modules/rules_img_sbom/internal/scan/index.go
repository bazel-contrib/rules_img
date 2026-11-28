package scan

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	stereoscopeFile "github.com/anchore/stereoscope/pkg/file"
	"github.com/anchore/stereoscope/pkg/filetree"
)

const (
	whiteoutPrefix = ".wh."
	whiteoutOpaque = ".wh..wh..opq"
)

// layerIndex is the file tree of one layer, with the content of its regular
// files spooled to a temporary file, so the scanner can read any of them at any
// time (catalogers run concurrently, in no particular order) after reading the
// layer's tar once.
type layerIndex struct {
	resolver   *resolver
	spool      *os.File
	paths      []string
	whiteouts  []string
	opaqueDirs []string
}

func (l *layerIndex) Close() error {
	return errors.Join(l.spool.Close(), os.Remove(l.spool.Name()))
}

// indexTar reads a layer's uncompressed tar. Whiteouts are not files of the
// layer: they are recorded, as the paths they delete from the layers below. The
// last entry for a path wins, as when the layer is applied.
func indexTar(r io.Reader, tempDir string) (_ *layerIndex, err error) {
	spool, err := os.CreateTemp(tempDir, "layer-*.spool")
	if err != nil {
		return nil, err
	}
	l := &layerIndex{spool: spool}
	defer func() {
		if err != nil {
			l.Close()
		}
	}()

	type entry struct {
		hdr    tar.Header
		offset int64
		ok     bool // has content in the spool
	}
	entries := map[string]*entry{}
	var order []string
	var offset int64
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the layer: %w", err)
		}
		name := path.Clean("/" + hdr.Name)
		base := path.Base(name)
		switch {
		case base == whiteoutOpaque:
			l.opaqueDirs = append(l.opaqueDirs, path.Dir(name))
			continue
		case strings.HasPrefix(base, whiteoutPrefix):
			l.whiteouts = append(l.whiteouts, path.Join(path.Dir(name), strings.TrimPrefix(base, whiteoutPrefix)))
			continue
		case name == "/":
			continue
		}

		e := &entry{hdr: *hdr}
		e.hdr.Name = name
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			e.hdr.Typeflag = tar.TypeReg
			n, err := io.Copy(spool, tr)
			if err != nil {
				return nil, fmt.Errorf("reading %s: %w", name, err)
			}
			e.offset, e.hdr.Size, e.ok = offset, n, true
			offset += n
		case tar.TypeLink:
			// A second name of a regular file earlier in the layer: index it as
			// that file, with its own name and permissions.
			target, found := entries[path.Clean("/"+hdr.Linkname)]
			if !found || !target.ok {
				continue
			}
			e.hdr.Typeflag = tar.TypeReg
			e.hdr.Linkname = ""
			e.hdr.Size = target.hdr.Size
			e.offset, e.ok = target.offset, true
		}
		if _, seen := entries[name]; !seen {
			order = append(order, name)
		}
		entries[name] = e
	}

	tree := filetree.New()
	index := filetree.NewIndex()
	builder := filetree.NewBuilder(tree, index)
	openers := map[stereoscopeFile.ID]func() (io.ReadCloser, error){}
	for _, name := range order {
		e := entries[name]
		var content io.Reader
		if e.ok {
			content = io.NewSectionReader(spool, e.offset, e.hdr.Size)
		}
		metadata := stereoscopeFile.NewMetadata(e.hdr, content)
		ref, err := builder.Add(metadata)
		if err != nil {
			// A path that conflicts with another (a file below a symlink, ...)
			// can't be part of the tree; skip it, as syft does.
			continue
		}
		if e.ok {
			offset, size := e.offset, e.hdr.Size
			openers[ref.ID()] = func() (io.ReadCloser, error) {
				return io.NopCloser(io.NewSectionReader(spool, offset, size)), nil
			}
		}
		if e.hdr.Typeflag != tar.TypeDir {
			l.paths = append(l.paths, name)
		}
	}
	sort.Strings(l.paths)
	sort.Strings(l.whiteouts)
	sort.Strings(l.opaqueDirs)
	l.resolver = &resolver{
		tree:    tree,
		index:   index,
		search:  filetree.NewSearchContext(tree, index),
		openers: openers,
	}
	return l, nil
}
