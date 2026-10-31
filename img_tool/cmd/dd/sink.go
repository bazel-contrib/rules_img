package ddcmd

import (
	"io"
	"os"
	"sync/atomic"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/extentstream"
)

// zeros is what holes are written from if the output is not sparse.
var zeros = make([]byte, 64<<10)

// sink is what the bytes of the image are written to. It collects them into
// blocks of bs bytes and, if it is sparse, seeks over holes and blocks of zeros
// instead of writing them. It implements extentstream.HoleWriter.
type sink struct {
	f       *os.File
	seekOK  bool // the output can be seeked (a file or a device)
	sparse  bool
	regular bool // a regular file: its size is set at the end
	buf     []byte
	n       int

	start   int64 // offset of the first byte
	pending int64 // bytes skipped over, not yet seeked over

	advanced atomic.Int64 // bytes of the image handled, for progress
	written  int64        // bytes actually written
	skipped  int64        // bytes seeked over
}

func newSink(f *os.File, bs int64, start int64, seekOK, sparse, regular bool) *sink {
	return &sink{f: f, seekOK: seekOK, sparse: sparse, regular: regular, buf: make([]byte, bs), start: start}
}

func (s *sink) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		c := copy(s.buf[s.n:], p)
		s.n += c
		p = p[c:]
		if s.n == len(s.buf) {
			if err := s.flush(); err != nil {
				return 0, err
			}
		}
	}
	s.advanced.Add(int64(total))
	return total, nil
}

// WriteHole advances by n bytes of zeros.
func (s *sink) WriteHole(n int64) error {
	if s.sparse {
		if err := s.flush(); err != nil {
			return err
		}
		s.pending += n
		s.skipped += n
		s.advanced.Add(n)
		return nil
	}
	for left := n; left > 0; {
		c := min(left, int64(len(zeros)))
		if _, err := s.Write(zeros[:c]); err != nil {
			return err
		}
		left -= c
	}
	return nil
}

// flush writes the buffered block, or skips it if it is all zeros.
func (s *sink) flush() error {
	if s.n == 0 {
		return nil
	}
	b := s.buf[:s.n]
	s.n = 0
	if s.sparse && extentstream.IsZero(b) {
		s.pending += int64(len(b))
		s.skipped += int64(len(b))
		return nil
	}
	if s.pending > 0 {
		if _, err := s.f.Seek(s.pending, io.SeekCurrent); err != nil {
			return err
		}
		s.pending = 0
	}
	n, err := s.f.Write(b)
	s.written += int64(n)
	return err
}

// finish writes what is buffered and settles the end of the output: a file that
// ends in skipped bytes is extended to its full size.
func (s *sink) finish() error {
	if err := s.flush(); err != nil {
		return err
	}
	if s.pending > 0 && s.regular {
		end := s.start + s.advanced.Load()
		if st, err := s.f.Stat(); err == nil && st.Size() < end {
			if err := s.f.Truncate(end); err != nil {
				return err
			}
		}
	}
	s.pending = 0
	return nil
}
