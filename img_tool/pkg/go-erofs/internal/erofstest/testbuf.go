package erofstest

import (
	"fmt"
	"io"
)

// TestBuffer is an in-memory io.WriteSeeker for tests.
type TestBuffer struct {
	buf []byte
	pos int
}

func (b *TestBuffer) Write(p []byte) (int, error) {
	end := b.pos + len(p)
	if end > len(b.buf) {
		if end > cap(b.buf) {
			newBuf := make([]byte, end, end*2)
			copy(newBuf, b.buf)
			b.buf = newBuf
		} else {
			b.buf = b.buf[:end]
		}
	}
	copy(b.buf[b.pos:], p)
	b.pos = end
	return len(p), nil
}

func (b *TestBuffer) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = int64(b.pos) + offset
	case io.SeekEnd:
		abs = int64(len(b.buf)) + offset
	default:
		return 0, fmt.Errorf("testbuf: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("testbuf: negative position %d", abs)
	}
	b.pos = int(abs)
	return abs, nil
}

func (b *TestBuffer) Bytes() []byte {
	return b.buf
}
