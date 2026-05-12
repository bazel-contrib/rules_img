package tarcas

import (
	"archive/tar"
	"hash"
	"io"
)

type EntryObserver interface {
	BeginEntry(hdr *tar.Header, knownDigest []byte, localPathHint string) (contentWriter io.Writer, err error)
	EndEntry() error
	Close() error
}

type indexObserver[HM hashHelper] struct {
	indexWriter     *IndexWriter
	hasher          hash.Hash
	hdr             *tar.Header
	rawHeader       []byte
	knownDigest     []byte
	localPathHint       string
	inlineThreshold int64
	inline          bool
}

func newIndexObserver[HM hashHelper](iw *IndexWriter) *indexObserver[HM] {
	return &indexObserver[HM]{
		indexWriter:     iw,
		inlineThreshold: iw.InlineThreshold(),
	}
}

func (o *indexObserver[HM]) BeginEntry(hdr *tar.Header, knownDigest []byte, localPathHint string) (io.Writer, error) {
	o.hasher = nil
	o.knownDigest = knownDigest
	o.inline = false
	o.hdr = hdr
	o.localPathHint = localPathHint

	rawHeader, err := captureTarHeaderBytes(hdr)
	if err != nil {
		return nil, err
	}
	o.rawHeader = rawHeader

	if err := o.indexWriter.WriteStreamBytes(rawHeader); err != nil {
		return nil, err
	}

	if hdr.Typeflag != tar.TypeReg || hdr.Size == 0 {
		return nil, nil
	}

	if o.inlineThreshold > 0 && hdr.Size < o.inlineThreshold {
		o.inline = true
		return &streamBytesWriter{w: o.indexWriter}, nil
	}

	if knownDigest == nil {
		var helper HM
		o.hasher = helper.New()
		return o.hasher, nil
	}
	return nil, nil
}

func (o *indexObserver[HM]) EndEntry() error {
	defer func() {
		o.hasher = nil
		o.rawHeader = nil
		o.knownDigest = nil
		o.hdr = nil
		o.localPathHint = ""
		o.inline = false
	}()

	if o.inline {
		if o.hdr != nil && o.hdr.Typeflag == tar.TypeReg && o.hdr.Size > 0 {
			padding := tarBlockPadding(o.hdr.Size)
			if len(padding) > 0 {
				return o.indexWriter.WriteStreamBytes(padding)
			}
		}
		return nil
	}

	if o.hdr == nil || o.hdr.Typeflag != tar.TypeReg || o.hdr.Size == 0 {
		return nil
	}

	digest := o.knownDigest
	if digest == nil && o.hasher != nil {
		digest = o.hasher.Sum(nil)
	}
	if digest == nil {
		return nil
	}

	if err := o.indexWriter.WriteCASRef(digest, uint64(o.hdr.Size), o.localPathHint); err != nil {
		return err
	}

	padding := tarBlockPadding(o.hdr.Size)
	if len(padding) > 0 {
		return o.indexWriter.WriteStreamBytes(padding)
	}
	return nil
}

func (o *indexObserver[HM]) Close() error {
	return o.indexWriter.Close()
}

type streamBytesWriter struct {
	w *IndexWriter
}

func (s *streamBytesWriter) Write(p []byte) (int, error) {
	if err := s.w.WriteStreamBytes(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func tarBlockPadding(size int64) []byte {
	remainder := size % 512
	if remainder == 0 {
		return nil
	}
	return make([]byte, 512-remainder)
}
