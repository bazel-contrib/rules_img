package erofs

import (
	"bytes"
	"fmt"
	"io"
	"math/bits"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Zstandard physical clusters: a cluster holds one whole zstd frame, right-aligned
// behind zero padding like every compressed cluster. The encoder is the pure Go
// one with fixed settings, so its output is a pure function of the input for a
// given version of the library; a compact stream records the digest of what it
// produced and refuses to reconstruct anything else.

const (
	zstdWindowLogMax = 20
	// zstdWindowLogMin is ZSTD_WINDOWLOG_ABSOLUTEMIN, which the superblock's
	// window size is stored relative to.
	zstdWindowLogMin = 10
)

type zstdEncoder struct {
	enc    *zstd.Encoder
	search fitSearch
}

// zstdWindowLog is the window log that covers the most input one cluster holds.
func zstdWindowLog(maxExtent int) int {
	return min(max(bits.Len(uint(maxExtent-1)), zstdWindowLogMin), zstdWindowLogMax)
}

func newZstdEncoder(windowLog int) (*zstdEncoder, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(false),
		zstd.WithWindowSize(1<<windowLog),
	)
	if err != nil {
		return nil, err
	}
	return &zstdEncoder{enc: enc}, nil
}

// encode compresses the longest prefix of src it finds whose frame fits budget
// bytes. final is unused: a frame has no end-of-block rules.
func (e *zstdEncoder) encode(src []byte, budget int, _ bool) ([]byte, int) {
	return e.search.search(src, budget, func(dst, src []byte) ([]byte, bool) {
		return e.enc.EncodeAll(src, dst), true
	})
}

var zstdDecoders = sync.Pool{New: func() any {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(1<<20))
	if err != nil {
		panic(err) // only invalid options fail
	}
	return dec
}}

// zstdDecode decodes the frame that makes up in into dst, which must be
// exactly the size of the decoded data.
func zstdDecode(dst, in []byte) error {
	dec := zstdDecoders.Get().(*zstd.Decoder)
	defer zstdDecoders.Put(dec)
	if err := dec.Reset(bytes.NewReader(in)); err != nil {
		return err
	}
	if _, err := io.ReadFull(dec, dst); err != nil {
		return fmt.Errorf("corrupt zstd frame: %w", err)
	}
	return nil
}
