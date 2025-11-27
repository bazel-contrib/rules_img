package tarcas

import "archive/tar"

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
}

type CreateParentDirectories bool

func (s FileStructure) apply(opts *options) { opts.structure = s }

func (f WriteHeaderCallback) apply(opts *options) { opts.writeHeaderCallback = f }

func (f WriteHeaderCallbackFilter) apply(opts *options) { opts.writeHeaderCallbackFilter = f }

func (c CreateParentDirectories) apply(opts *options) { opts.createParentDirectories = bool(c) }
