package erofs

import (
	"encoding/binary"
	"errors"
)

// A minimal, deterministic LZ4 block encoder.
//
// EROFS stores each compressed extent as a bare LZ4 block, and the extent has
// to fit a fixed number of bytes (one filesystem block here), so what is needed
// is "compress as much input as fits into this many bytes" rather than "compress
// this input". The encoder is greedy (no lazy matching, no acceleration) and has
// no tunables, which keeps its output a pure function of the input: the same
// bytes always produce the same block, on every platform and Go version.
// Compression ratio is secondary to that, because a compact stream reproduces
// the output by running this encoder again.
//
// Blocks follow the LZ4 block format: the last match starts at least 12 bytes
// before the end of the block and the last 5 bytes are literals.

const (
	lz4MinMatch     = 4
	lz4MFLimit      = 12 // a match must start at least this far from the block end
	lz4LastLiterals = 5  // the block ends with at least this many literals
	lz4MaxDistance  = 65535
	lz4HashLog      = 12
	// lz4TailLiterals is how many input bytes a truncated block keeps as
	// trailing literals after its last match. It exceeds lz4MFLimit so that
	// the last match always satisfies both end-of-block rules.
	lz4TailLiterals = lz4MFLimit
)

type lz4Encoder struct {
	table [1 << lz4HashLog]int32 // hash of 4 bytes -> position+1 (0 = empty)
	// seqs records the state after each committed sequence so that a cut can
	// back off to an earlier one.
	seqs []lz4State
	out  []byte
}

type lz4State struct {
	outLen int // len(out) after the sequence
	anchor int // input position just past the sequence's match
}

func lz4Hash(v uint32) uint32 { return (v * 2654435761) >> (32 - lz4HashLog) }

func lz4LitExtra(n int) int {
	if n < 15 {
		return 0
	}
	return (n-15)/255 + 1
}

func lz4AppendLen(dst []byte, n int) []byte {
	// n is the part above the 15 stored in the token.
	for n >= 255 {
		dst = append(dst, 255)
		n -= 255
	}
	return append(dst, byte(n))
}

// encode compresses a prefix of src into at most budget bytes and returns the
// block and the number of input bytes it covers. final reports that src ends at
// the end of the data being encoded, which allows the block to cover all of it
// and to finish on a short run of literals.
//
// The returned slice is only valid until the next call. n is 0 when nothing
// useful fits.
func (e *lz4Encoder) encode(src []byte, budget int, final bool) (block []byte, n int) {
	clear(e.table[:])
	e.seqs = e.seqs[:0]
	e.out = e.out[:0]

	anchor := 0
	pos := 0
	// Matches may only start at least lz4MFLimit before the end of src and end
	// at least lz4LastLiterals before it.
	limit := len(src) - lz4MFLimit
	exhausted := true
	for pos < limit {
		seq := binary.LittleEndian.Uint32(src[pos:])
		h := lz4Hash(seq)
		cand := int(e.table[h]) - 1
		e.table[h] = int32(pos + 1)
		if cand < 0 || pos-cand > lz4MaxDistance || binary.LittleEndian.Uint32(src[cand:]) != seq {
			pos++
			continue
		}
		maxEnd := len(src) - lz4LastLiterals
		if !final {
			// A block cut short must still have room for its trailing literals.
			maxEnd = len(src) - lz4TailLiterals
		}
		mlen := lz4MinMatch
		for pos+mlen < maxEnd && src[cand+mlen] == src[pos+mlen] {
			mlen++
		}
		lits := pos - anchor
		cost := 1 + lz4LitExtra(lits) + lits + 2 + lz4LitExtra(mlen-lz4MinMatch)
		end := pos + mlen
		// What the block needs after this sequence: either the rest of the
		// data as literals (when it ends the data) or a fixed run of them.
		tail := lz4TailLiterals
		if final && len(src)-end < tail {
			tail = len(src) - end
		}
		if len(e.out)+cost+1+tail > budget {
			exhausted = false
			break
		}
		e.out = lz4AppendSequence(e.out, src[anchor:pos], pos-cand, mlen)
		anchor = end
		e.seqs = append(e.seqs, lz4State{outLen: len(e.out), anchor: anchor})
		// Index a position inside the match so later data can refer to it.
		if end-2 > pos {
			e.table[lz4Hash(binary.LittleEndian.Uint32(src[end-2:]))] = int32(end - 2 + 1)
		}
		pos = end
	}

	if exhausted && final {
		// The whole of src was parsed: finish with everything left as literals.
		rest := len(src) - anchor
		if len(e.out)+1+lz4LitExtra(rest)+rest <= budget {
			e.out = lz4AppendLiterals(e.out, src[anchor:])
			return e.out, len(src)
		}
	}

	// Cut: keep as many sequences as leave room for a trailing run of literals,
	// then fill the rest of the budget with literals.
	for {
		outLen, anchor := 0, 0
		if k := len(e.seqs); k > 0 {
			outLen, anchor = e.seqs[k-1].outLen, e.seqs[k-1].anchor
		}
		avail := len(src) - anchor
		// Largest t <= avail with 1 + extra(t) + t <= budget-outLen.
		room := budget - outLen
		t := min(avail, room-1)
		for t > 0 && 1+lz4LitExtra(t)+t > room {
			t--
		}
		if len(e.seqs) == 0 {
			if t <= 0 {
				return nil, 0
			}
		} else if t < lz4TailLiterals {
			e.seqs = e.seqs[:len(e.seqs)-1]
			continue
		}
		e.out = e.out[:outLen]
		e.out = lz4AppendLiterals(e.out, src[anchor:anchor+t])
		return e.out, anchor + t
	}
}

func lz4AppendSequence(dst, lits []byte, offset, mlen int) []byte {
	l := len(lits)
	m := mlen - lz4MinMatch
	tok := byte(0)
	if l >= 15 {
		tok = 15 << 4
	} else {
		tok = byte(l) << 4
	}
	if m >= 15 {
		tok |= 15
	} else {
		tok |= byte(m)
	}
	dst = append(dst, tok)
	if l >= 15 {
		dst = lz4AppendLen(dst, l-15)
	}
	dst = append(dst, lits...)
	dst = append(dst, byte(offset), byte(offset>>8))
	if m >= 15 {
		dst = lz4AppendLen(dst, m-15)
	}
	return dst
}

func lz4AppendLiterals(dst, lits []byte) []byte {
	l := len(lits)
	if l >= 15 {
		dst = append(dst, 15<<4)
		dst = lz4AppendLen(dst, l-15)
	} else {
		dst = append(dst, byte(l)<<4)
	}
	return append(dst, lits...)
}

// lz4Decode decodes the LZ4 block in into dst, which must be exactly the size of
// the decoded data. It bounds-checks every copy, so a corrupt block is an error
// and never a panic or an overrun.
func lz4Decode(dst, in []byte) error {
	o := 0
	for i := 0; ; {
		if i >= len(in) {
			return errLZ4Corrupt
		}
		tok := in[i]
		i++
		lits := int(tok >> 4)
		if lits == 15 {
			for {
				if i >= len(in) {
					return errLZ4Corrupt
				}
				b := in[i]
				i++
				lits += int(b)
				if b != 255 {
					break
				}
			}
		}
		if lits > len(in)-i || lits > len(dst)-o {
			return errLZ4Corrupt
		}
		copy(dst[o:], in[i:i+lits])
		i += lits
		o += lits
		if i == len(in) {
			if o != len(dst) {
				return errLZ4Corrupt
			}
			return nil
		}
		if i+2 > len(in) {
			return errLZ4Corrupt
		}
		off := int(in[i]) | int(in[i+1])<<8
		i += 2
		mlen := int(tok & 15)
		if mlen == 15 {
			for {
				if i >= len(in) {
					return errLZ4Corrupt
				}
				b := in[i]
				i++
				mlen += int(b)
				if b != 255 {
					break
				}
			}
		}
		mlen += lz4MinMatch
		if off == 0 || off > o || mlen > len(dst)-o {
			return errLZ4Corrupt
		}
		for j := 0; j < mlen; j++ {
			dst[o+j] = dst[o+j-off]
		}
		o += mlen
	}
}

var errLZ4Corrupt = errors.New("corrupt LZ4 block")
