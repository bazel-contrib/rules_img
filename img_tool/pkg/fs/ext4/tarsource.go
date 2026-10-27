package ext4

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
)

// TarArchive is a tar stream that can be opened again. The Builder uses it to
// read the content of regular files when the image is written, instead of
// keeping a copy of the content until then.
//
// The Builder keeps one open reader per archive, so an implementation has to be
// comparable (Go panics on a map key that is not): a pointer, or a value of a
// type without fields that cannot be compared. Two values that compare equal
// must describe the same archive, and are then read only once.
type TarArchive interface {
	// Open starts reading the archive from its first entry. The returned
	// closer releases the reader.
	Open() (*tar.Reader, io.Closer, error)
}

// WriteRegularFromTarEntry adds the regular file that is the index-th entry
// (counting from zero, over all entries) of ar. Nothing is read now: the
// content is read from the archive during Build. Entries are streamed in the
// order in which they were added, so adding the entries of an archive in
// archive order reads it exactly once. The archive must not change before
// then.
//
// With [Options.Deduplicate] the file is merged with identical files, like
// [Builder.WriteRegularDeduplicatedKnownHash] (knownHash may be nil).
func (b *Builder) WriteRegularFromTarEntry(hdr *tar.Header, ar TarArchive, index int, knownHash []byte) error {
	if !isRegular(hdr.Typeflag) {
		return fmt.Errorf("ext4: %s: type %q is not a regular file", hdr.Name, hdr.Typeflag)
	}
	if hdr.Size < 0 {
		return fmt.Errorf("ext4: %s: negative size", hdr.Name)
	}
	if hdr.Size == 0 {
		return b.add(hdr, memSource(nil), false, nil)
	}
	return b.add(hdr, &tarEntrySource{cursors: &b.tars, archive: ar, index: index, size: hdr.Size}, b.opts.Deduplicate, sha256Digest(knownHash))
}

type tarEntrySource struct {
	cursors *tarCursors
	archive TarArchive
	index   int
	size    int64
}

func (s *tarEntrySource) open() (io.ReadCloser, error) {
	r, err := s.cursors.entry(s.archive, s.index, s.size)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(r), nil
}

func (*tarEntrySource) release() {}

// tarCursors hold one open reader per archive. Entries that are requested in
// archive order are reached by reading forward; asking for an earlier entry
// starts the archive over.
type tarCursors struct {
	open map[TarArchive]*tarCursor
}

type tarCursor struct {
	tr     *tar.Reader
	closer io.Closer
	// index is the index of the entry the reader is positioned in.
	index int
	// used reports whether reading of that entry has been handed out.
	used bool
}

// entry returns a reader for the content of an entry.
func (c *tarCursors) entry(ar TarArchive, index int, size int64) (io.Reader, error) {
	cur := c.open[ar]
	if cur != nil && (index < cur.index || (index == cur.index && cur.used)) {
		cur.closer.Close()
		delete(c.open, ar)
		cur = nil
	}
	if cur == nil {
		tr, closer, err := ar.Open()
		if err != nil {
			return nil, fmt.Errorf("reopening tar archive: %w", err)
		}
		cur = &tarCursor{tr: tr, closer: closer, index: -1}
		if c.open == nil {
			c.open = map[TarArchive]*tarCursor{}
		}
		c.open[ar] = cur
	}
	for cur.index < index {
		hdr, err := cur.tr.Next()
		if err != nil {
			return nil, fmt.Errorf("tar archive changed: entry %d is missing: %w", index, unexpected(err))
		}
		cur.index++
		cur.used = false
		if cur.index == index && (!isRegular(hdr.Typeflag) || hdr.Size != size) {
			return nil, fmt.Errorf("tar archive changed: entry %d (%s) is not a regular file of %d bytes", index, hdr.Name, size)
		}
	}
	cur.used = true
	return cur.tr, nil
}

func (c *tarCursors) close() error {
	var errs []error
	for ar, cur := range c.open {
		errs = append(errs, cur.closer.Close())
		delete(c.open, ar)
	}
	return errors.Join(errs...)
}
