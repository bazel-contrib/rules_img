package erofs

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
)

// DEFLATE physical clusters: one raw DEFLATE stream (no zlib or gzip framing),
// right-aligned behind zero padding like every compressed cluster. The encoder is
// the standard library's with a fixed level, so its output is a pure function of
// the input for a given version of Go; a compact stream records the digest of what
// it produced and refuses to reconstruct anything else.

const (
	// deflateWindowBits is the window the superblock advertises: the 32 KiB
	// window of DEFLATE.
	deflateWindowBits = 15
	deflateLevel      = flate.DefaultCompression
)

type deflateEncoder struct {
	w      *flate.Writer
	out    bytes.Buffer
	search fitSearch
}

func newDeflateEncoder() (*deflateEncoder, error) {
	e := &deflateEncoder{}
	w, err := flate.NewWriter(&e.out, deflateLevel)
	if err != nil {
		return nil, err
	}
	e.w = w
	return e, nil
}

// encode compresses the longest prefix of src it finds whose stream fits budget
// bytes. final is unused: a stream has no end-of-block rules.
func (e *deflateEncoder) encode(src []byte, budget int, _ bool) ([]byte, int) {
	return e.search.search(src, budget, func(dst, src []byte) ([]byte, bool) {
		e.out.Reset()
		e.w.Reset(&e.out)
		if _, err := e.w.Write(src); err != nil {
			return dst, false
		}
		if err := e.w.Close(); err != nil {
			return dst, false
		}
		// A leading zero byte would be taken for padding. The stream starts with
		// one when its first block is a stored block that is not the last.
		if e.out.Len() == 0 || e.out.Bytes()[0] == 0 {
			return dst, false
		}
		return append(dst, e.out.Bytes()...), true
	})
}

// deflateDecode decodes the raw DEFLATE stream in into dst, which must be
// exactly the size of the decoded data.
func deflateDecode(dst, in []byte) error {
	r := flate.NewReader(bytes.NewReader(in))
	defer r.Close()
	if _, err := io.ReadFull(r, dst); err != nil {
		return fmt.Errorf("corrupt DEFLATE stream: %w", err)
	}
	return nil
}
