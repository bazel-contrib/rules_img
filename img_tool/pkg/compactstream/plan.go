package compactstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// Region is a byte range of the reconstructed stream that is replaced by a
// single CAS reference rather than carried inline in the byte stream.
type Region struct {
	// Offset is the range's byte offset in the reconstructed (uncompressed)
	// stream.
	Offset int64
	// Size is the range's length in bytes.
	Size int64
	// Digest is the content digest of the referenced blob, which must be the
	// digest of exactly the bytes in the range, or of the blob they are derived
	// from when Transform is set.
	Digest []byte
	// Transform, when set, says the bytes in the range are derived from the blob
	// instead of being the blob. Its SourceSize and Kind are required; the
	// splitter fills in OutputDigest from the bytes it sees.
	Transform *Transform
}

// PlanSplitter is an io.Writer that forwards every byte to a downstream writer
// while splitting the same bytes into a compact stream according to a plan
// computed ahead of time: bytes outside any region become inline stream bytes,
// and each region contributes exactly one CAS reference.
//
// It exists for producers that know the shape of their output before they emit
// it -- an EROFS layer, whose layout is planned up front and whose file payloads
// are contiguous, block-aligned copies of the input files -- as opposed to the
// tar path, which discovers the shape entry by entry as it writes.
type PlanSplitter struct {
	writer  *Writer
	next    io.Writer
	regions []Region
	verify  bool

	pos       int64     // bytes seen so far
	idx       int       // index of the region being consumed, or the next one
	remaining int64     // bytes left in the region being consumed
	hasher    hash.Hash // digest of the region being consumed, when verifying
	err       error
}

// NewPlanSplitter returns a splitter that forwards every byte to next (the
// compressor and hashing pipeline) and simultaneously drives w. Regions must be
// sorted by offset, non-overlapping, and carry digests of the index's hash size.
//
// When verify is true the splitter hashes each region's bytes as they pass and
// fails if they do not match the recorded digest. That is worth its cost: the
// bytes are already in hand, a sha256 pass is cheap next to compression, and it
// turns a wrong plan into an immediate build failure instead of a corrupt
// compact stream that only fails when someone tries to reconstruct the layer.
func NewPlanSplitter(w *Writer, next io.Writer, regions []Region, verify bool) (*PlanSplitter, error) {
	if verify && w.hashAlgo != HashAlgoSHA256 {
		return nil, fmt.Errorf("compact stream: verifying a plan supports only sha256 (algorithm %d)", w.hashAlgo)
	}
	var prevEnd int64
	for i, r := range regions {
		if r.Size <= 0 {
			return nil, fmt.Errorf("compact stream: region %d has non-positive size %d", i, r.Size)
		}
		if r.Offset < prevEnd {
			return nil, fmt.Errorf("compact stream: region %d at offset %d overlaps or precedes the previous region ending at %d", i, r.Offset, prevEnd)
		}
		if r.Transform != nil {
			if err := r.Transform.validate(); err != nil {
				return nil, fmt.Errorf("region %d: %w", i, err)
			}
		}
		if len(r.Digest) != w.hashSize {
			return nil, fmt.Errorf("compact stream: region %d digest length %d does not match expected hash size %d", i, len(r.Digest), w.hashSize)
		}
		prevEnd = r.Offset + r.Size
	}
	return &PlanSplitter{writer: w, next: next, regions: regions, verify: verify}, nil
}

func (s *PlanSplitter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	total := len(p)

	// The downstream pipeline sees the byte stream verbatim, so it computes the
	// uncompressed digest (the layer's diff ID) and the compressed blob over
	// exactly the bytes the plan describes.
	if _, err := s.next.Write(p); err != nil {
		return s.fail(err)
	}

	for len(p) > 0 {
		if s.remaining > 0 {
			n := int64(len(p))
			if n > s.remaining {
				n = s.remaining
			}
			if s.hasher != nil {
				s.hasher.Write(p[:n])
			}
			s.pos += n
			s.remaining -= n
			p = p[n:]
			if s.remaining == 0 {
				if err := s.endRegion(); err != nil {
					return s.fail(err)
				}
			}
			continue
		}

		// Outside a region: carry bytes inline, stopping exactly at the next
		// region boundary.
		limit := int64(len(p))
		if s.idx < len(s.regions) {
			gap := s.regions[s.idx].Offset - s.pos
			if gap == 0 {
				if err := s.beginRegion(); err != nil {
					return s.fail(err)
				}
				continue
			}
			if gap < limit {
				limit = gap
			}
		}
		if err := s.writer.WriteStreamBytes(p[:limit]); err != nil {
			return s.fail(err)
		}
		s.pos += limit
		p = p[limit:]
	}
	return total, nil
}

// Finish reports whether the bytes written so far match the plan: every region
// must have been consumed in full and the total must equal size. A short or
// overlong stream means the plan and the producer disagree, which would leave
// the compact stream unable to reconstruct the blob.
func (s *PlanSplitter) Finish(size int64) error {
	if s.err != nil {
		return s.err
	}
	if s.pos != size {
		return fmt.Errorf("compact stream: split %d bytes, plan describes %d", s.pos, size)
	}
	if s.idx != len(s.regions) {
		return fmt.Errorf("compact stream: %d of %d regions were never reached (stream ended at offset %d)", len(s.regions)-s.idx, len(s.regions), s.pos)
	}
	return nil
}

func (s *PlanSplitter) beginRegion() error {
	r := s.regions[s.idx]
	var err error
	if r.Transform != nil {
		err = s.writer.WriteTransformedCASRef(r.Digest, uint64(r.Size), *r.Transform)
	} else {
		err = s.writer.WriteCASRef(r.Digest, uint64(r.Size))
	}
	if err != nil {
		return err
	}
	s.remaining = r.Size
	// A transformed region's digest is what the stream records, so it is always
	// needed; a plain one is only hashed to check the plan.
	if s.verify || r.Transform != nil {
		s.hasher = sha256.New()
	}
	return nil
}

func (s *PlanSplitter) endRegion() error {
	r := s.regions[s.idx]
	if s.hasher != nil && r.Transform != nil {
		if err := s.writer.SetTransformOutputDigest(s.hasher.Sum(nil)); err != nil {
			return err
		}
		s.hasher = nil
	}
	if s.hasher != nil {
		if got := s.hasher.Sum(nil); !bytes.Equal(got, r.Digest) {
			return fmt.Errorf("compact stream: region at offset %d (%d bytes) hashes to %s, but the plan recorded %s",
				r.Offset, r.Size, hex.EncodeToString(got), hex.EncodeToString(r.Digest))
		}
		s.hasher = nil
	}
	s.idx++
	return nil
}

func (s *PlanSplitter) fail(err error) (int, error) {
	s.err = err
	return 0, err
}
