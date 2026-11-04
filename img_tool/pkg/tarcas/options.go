package tarcas

import (
	"archive/tar"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

type Option interface {
	apply(*options)
}

type FileStructure struct{ inner int }

var (
	CASFirst    = FileStructure{inner: 0}
	CASOnly     = FileStructure{inner: 1}
	Intertwined = FileStructure{inner: 2}
)

type WriteHeaderCallback (func(hdr *tar.Header) error)

// GeneratedMetadata applies metadata to tree entries and synthesized parent
// directories. Regular-file metadata is applied before deduplication.
type GeneratedMetadata func(hdr *tar.Header) error

type WriteHeaderCallbackFilter uint64

const (
	WriteHeaderCallbackNone WriteHeaderCallbackFilter = (1 << iota) >> 1
	WriteHeaderCallbackRegular
	WriteHeaderCallbackDir
	WriteHeaderCallbackLink
	WriteHeaderCallbackSymlink

	WriteHeaderCallbackFilterDefault = WriteHeaderCallbackDir | WriteHeaderCallbackLink | WriteHeaderCallbackSymlink
	WriteHeaderCallbackFilterAll     = WriteHeaderCallbackRegular | WriteHeaderCallbackDir | WriteHeaderCallbackLink | WriteHeaderCallbackSymlink
)

type options struct {
	structure                 FileStructure
	writeHeaderCallback       WriteHeaderCallback
	writeHeaderCallbackFilter WriteHeaderCallbackFilter
	createParentDirectories   bool
	endOfArchive              bool
	deduplicateTreeArtifacts  bool
	compactStreamWriter       *compactstream.Writer
	observer                  EntryObserver
	generatedMetadata         GeneratedMetadata
}

type CreateParentDirectories bool

// EndOfArchive makes Close terminate the archive with the two zero-filled
// 512-byte blocks that POSIX requires at the end of a tar archive.
// Without it the archive simply ends after the last entry, which Go's
// archive/tar and GNU tar accept, but readers that cannot tell the end of
// the archive without reading past it (libarchive) depend on the underlying
// stream to signal EOF cleanly.
//
// Observers see the blocks if they implement EndOfArchiveObserver.
type EndOfArchive bool

func (e EndOfArchive) apply(opts *options) { opts.endOfArchive = bool(e) }

func (s FileStructure) apply(opts *options) { opts.structure = s }

func (f WriteHeaderCallback) apply(opts *options) { opts.writeHeaderCallback = f }

func (m GeneratedMetadata) apply(opts *options) { opts.generatedMetadata = m }

func (f WriteHeaderCallbackFilter) apply(opts *options) { opts.writeHeaderCallbackFilter = f }

func (c CreateParentDirectories) apply(opts *options) { opts.createParentDirectories = bool(c) }

type DeduplicateTreeArtifacts bool

func (d DeduplicateTreeArtifacts) apply(opts *options) { opts.deduplicateTreeArtifacts = bool(d) }

type WithCompactStreamWriter struct{ Writer *compactstream.Writer }

func (w WithCompactStreamWriter) apply(opts *options) { opts.compactStreamWriter = w.Writer }

type WithEntryObserver struct{ Observer EntryObserver }

func (w WithEntryObserver) apply(opts *options) { opts.observer = w.Observer }
