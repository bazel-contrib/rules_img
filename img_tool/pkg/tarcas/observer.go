package tarcas

import (
	"archive/tar"
	"hash"
	"io"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
)

// EntryObserver is notified of each tar entry as it is written, in the exact
// order entries reach the tar. BeginEntry and EndEntry must be paired: an entry
// that begins but never completes (e.g. a mid-entry AppendTar error) leaves the
// compactStreamObserver's byte offset advanced by the header without the
// matching CAS ref/padding, desyncing the compact stream from the tar. Callers
// must therefore abandon the writer after any such error rather than finalizing
// it; the layer tool does so by treating the error as fatal (os.Exit).
type EntryObserver interface {
	BeginEntry(hdr *tar.Header, knownDigest []byte) (contentWriter io.Writer, err error)
	EndEntry() error
	Close() error
}

type compactStreamObserver[HM hashHelper] struct {
	compactStreamWriter *compactstream.Writer
	hasher              hash.Hash
	hdr                 *tar.Header
	rawHeader           []byte
	knownDigest         []byte
	inlineThreshold     int64
	inline              bool
}

func newCompactStreamObserver[HM hashHelper](iw *compactstream.Writer) *compactStreamObserver[HM] {
	return &compactStreamObserver[HM]{
		compactStreamWriter: iw,
		inlineThreshold:     iw.InlineThreshold(),
	}
}

func (o *compactStreamObserver[HM]) BeginEntry(hdr *tar.Header, knownDigest []byte) (io.Writer, error) {
	o.hasher = nil
	o.knownDigest = knownDigest
	o.inline = false
	o.hdr = hdr

	rawHeader, err := compactstream.CaptureTarHeaderBytes(hdr)
	if err != nil {
		return nil, err
	}
	o.rawHeader = rawHeader

	if err := o.compactStreamWriter.WriteStreamBytes(rawHeader); err != nil {
		return nil, err
	}

	if hdr.Typeflag != tar.TypeReg || hdr.Size == 0 {
		return nil, nil
	}

	if o.inlineThreshold > 0 && hdr.Size < o.inlineThreshold {
		o.inline = true
		return &streamBytesWriter{w: o.compactStreamWriter}, nil
	}

	if knownDigest == nil {
		var helper HM
		o.hasher = helper.New()
		return o.hasher, nil
	}
	return nil, nil
}

func (o *compactStreamObserver[HM]) EndEntry() error {
	defer func() {
		o.hasher = nil
		o.rawHeader = nil
		o.knownDigest = nil
		o.hdr = nil
		o.inline = false
	}()

	if o.inline {
		if o.hdr != nil && o.hdr.Typeflag == tar.TypeReg && o.hdr.Size > 0 {
			// Padding is derived from the declared hdr.Size, not the number of
			// bytes actually streamed. This is safe because producers validate
			// that the content length equals hdr.Size before writing (e.g.
			// digestfs and the buffering Store/StoreNode paths), and any residual
			// mismatch is caught by the compressed-stream digest check during
			// reconstruction. The non-inline CAS path below relies on the same
			// invariant.
			padding := tarBlockPadding(o.hdr.Size)
			if len(padding) > 0 {
				return o.compactStreamWriter.WriteStreamBytes(padding)
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

	if err := o.compactStreamWriter.WriteCASRef(digest, uint64(o.hdr.Size)); err != nil {
		return err
	}

	padding := tarBlockPadding(o.hdr.Size)
	if len(padding) > 0 {
		return o.compactStreamWriter.WriteStreamBytes(padding)
	}
	return nil
}

// Close is a no-op: the observer collects stream bytes and CAS references into
// the compactstream.Writer but does not own its lifecycle. The owner of the compactstream.Writer
// is responsible for calling its Close, which lets the owner first record
// optional information (such as the compressed-stream digest and size) that is
// only available after the surrounding compressor has been finalized.
func (o *compactStreamObserver[HM]) Close() error {
	return nil
}

type streamBytesWriter struct {
	w *compactstream.Writer
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
