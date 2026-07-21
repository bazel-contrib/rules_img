package ztoc

import (
	"errors"
	"fmt"
	"hash/crc32"
)

// This file is a pure-Go DEFLATE/gzip decompressor. The inflate core is a port
// of Mark Adler's puff.c (a compact, canonical reference implementation of
// RFC 1951 inflate), extended to:
//
//   - track the exact bit position in the compressed stream, and
//   - invoke a callback at every deflate block boundary carrying the 32 KiB
//     sliding window,
//
// which is what building a zran-style index (see zinfo.go) requires. Standard
// library flate/gzip expose neither block boundaries nor bit positions, so a
// bespoke inflater is necessary. puff.c is distributed under the zlib license.

const (
	// winSize is the DEFLATE sliding-window size: 32 KiB.
	winSize = 32768
	winMask = winSize - 1

	maxBits    = 15  // maximum bits in a Huffman code
	maxLCodes  = 286 // maximum number of literal/length codes
	maxDCodes  = 30  // maximum number of distance codes
	fixLCodes  = 288 // number of fixed literal/length codes
	endOfBlock = 256 // literal/length symbol marking end of a block

	// gzip FLG bits (RFC 1952).
	flagHCRC    = 1 << 1
	flagExtra   = 1 << 2
	flagName    = 1 << 3
	flagComment = 1 << 4
)

// errTruncated is returned when the compressed stream ends unexpectedly.
var errTruncated = errors.New("ztoc: unexpected end of compressed stream")

// Length base values and extra bits for length symbols 257..285 (RFC 1951 3.2.5).
var (
	lengthBase = [29]int{3, 4, 5, 6, 7, 8, 9, 10, 11, 13, 15, 17, 19, 23, 27, 31,
		35, 43, 51, 59, 67, 83, 99, 115, 131, 163, 195, 227, 258}
	lengthExtra = [29]int{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2,
		3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}

	distBase = [30]int{1, 2, 3, 4, 5, 7, 9, 13, 17, 25, 33, 49, 65, 97, 129, 193,
		257, 385, 513, 769, 1025, 1537, 2049, 3073, 4097, 6145, 8193, 12289, 16385, 24577}
	distExtra = [30]int{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6,
		7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}

	// Order in which code-length code lengths are read (RFC 1951 3.2.7).
	codeLengthOrder = [19]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}
)

// bitReader reads bits (LSB-first, as DEFLATE requires) from an in-memory
// compressed buffer while tracking the exact number of bits consumed. It is
// modeled on puff.c's bit accessor.
type bitReader struct {
	in    []byte
	incnt int    // index of the next unread byte in in
	hold  uint64 // bit accumulator; valid bits are the low nbits
	nbits uint   // number of valid bits currently in hold
}

// bits returns the next need bits (0..32) as an integer, LSB-first.
func (b *bitReader) bits(need uint) (int, error) {
	for b.nbits < need {
		if b.incnt >= len(b.in) {
			return 0, errTruncated
		}
		b.hold |= uint64(b.in[b.incnt]) << b.nbits
		b.incnt++
		b.nbits += 8
	}
	val := int(b.hold & ((1 << need) - 1))
	b.hold >>= need
	b.nbits -= need
	return val, nil
}

// align discards any bits remaining in the current byte, leaving the reader on
// a byte boundary. It is used before stored blocks and the gzip trailer.
func (b *bitReader) align() {
	drop := b.nbits & 7
	b.hold >>= drop
	b.nbits -= drop
}

// readByte reads one full byte. It requires the reader to be byte-aligned
// (nbits is a multiple of 8), which holds after align() and at stream start.
func (b *bitReader) readByte() (byte, error) {
	if b.nbits >= 8 {
		v := byte(b.hold & 0xff)
		b.hold >>= 8
		b.nbits -= 8
		return v, nil
	}
	if b.incnt >= len(b.in) {
		return 0, errTruncated
	}
	v := b.in[b.incnt]
	b.incnt++
	return v, nil
}

// offset returns the current logical byte offset and the number of unused
// (fractional) bits carried into the next byte. This matches zlib's zran
// accounting: whole unread bytes buffered in the accumulator are "put back",
// so bits is always 0..7 and offset == ceil(bitsConsumed/8).
func (b *bitReader) offset() (off int64, bits uint8) {
	return int64(b.incnt) - int64(b.nbits>>3), uint8(b.nbits & 7)
}

// eof reports whether all input bytes have been consumed. It requires the
// reader to be byte-aligned (used only after a gzip trailer).
func (b *bitReader) eof() bool {
	return b.incnt >= len(b.in) && b.nbits == 0
}

// window is the DEFLATE 32 KiB sliding window. It doubles as the LZ77 history
// for back-references and as the source of checkpoint snapshots.
type window struct {
	buf [winSize]byte
	pos int // index of the next byte to write
}

// put appends a single decoded byte to the window.
func (w *window) put(b byte) {
	w.buf[w.pos] = b
	w.pos = (w.pos + 1) & winMask
}

// byteAt returns the byte dist positions back from the current write position
// (dist in 1..winSize).
func (w *window) byteAt(dist int) byte {
	return w.buf[(w.pos-dist)&winMask]
}

// snapshot fills out with the winSize uncompressed bytes immediately preceding
// total, zero-padded at the front when fewer than winSize bytes have been
// produced. This matches the checkpoint window layout expected by soci.
func (w *window) snapshot(out *[winSize]byte, total int64) {
	stored := total
	if stored > winSize {
		stored = winSize
	}
	for i := 0; i < winSize; i++ {
		back := winSize - i // distance from the write position (1..winSize)
		if int64(back) > stored {
			out[i] = 0
		} else {
			out[i] = w.buf[(w.pos-back)&winMask]
		}
	}
}

// huffman is a canonical Huffman decode table in puff.c's count/symbol form.
type huffman struct {
	count  [maxBits + 1]int // count[n] = number of codes of length n
	symbol []int            // symbols, ordered by (code length, then symbol)
}

// construct builds a Huffman decode table from a list of code lengths. It
// returns the "left" value: 0 for a complete code, >0 for an incomplete code,
// and <0 for an over-subscribed (invalid) code, mirroring puff.c.
func construct(h *huffman, lengths []int) int {
	for i := range h.count {
		h.count[i] = 0
	}
	for _, l := range lengths {
		h.count[l]++
	}
	if h.count[0] == len(lengths) {
		return 0 // no codes at all: complete (vacuously)
	}
	left := 1
	for l := 1; l <= maxBits; l++ {
		left <<= 1
		left -= h.count[l]
		if left < 0 {
			return left // over-subscribed
		}
	}
	var offs [maxBits + 1]int
	for l := 1; l < maxBits; l++ {
		offs[l+1] = offs[l] + h.count[l]
	}
	if cap(h.symbol) < len(lengths) {
		h.symbol = make([]int, len(lengths))
	} else {
		h.symbol = h.symbol[:len(lengths)]
	}
	for sym, l := range lengths {
		if l != 0 {
			h.symbol[offs[l]] = sym
			offs[l]++
		}
	}
	return left
}

// decode reads one symbol from the bit stream using table h (puff.c's decode).
func (inf *inflater) decode(h *huffman) (int, error) {
	code, first, index := 0, 0, 0
	for l := 1; l <= maxBits; l++ {
		bit, err := inf.br.bits(1)
		if err != nil {
			return 0, err
		}
		code |= bit
		count := h.count[l]
		if code-count < first {
			return h.symbol[index+(code-first)], nil
		}
		index += count
		first += count
		first <<= 1
		code <<= 1
	}
	return 0, errors.New("ztoc: invalid Huffman code")
}

// inflater drives gzip framing and deflate block decoding while maintaining the
// sliding window and firing onBoundary before each block header is read.
type inflater struct {
	br  bitReader
	win window

	total int64 // total uncompressed bytes produced across all members

	// memberStart is inf.total at the start of the current gzip member; a
	// distance back-reference may not reach before it (each member is an
	// independent deflate stream, as zlib enforces).
	memberStart int64

	// Per-member gzip integrity state. crc holds the running IEEE CRC32 in its
	// pre-conditioned (bit-inverted) form; finalize with ^crc.
	crc       uint32
	memberLen uint32 // member uncompressed length mod 2^32

	// fixed Huffman tables, built lazily and reused.
	fixedLen  *huffman
	fixedDist *huffman

	// scratch tables for dynamic blocks.
	dynLen  huffman
	dynDist huffman

	// onBoundary is called once at each deflate block boundary: before the
	// first block of the stream (after the gzip header) and before every
	// subsequent block. It is never called after the final block. It reports
	// the boundary's uncompressed offset via inf.total and its compressed
	// position via inf.br.offset().
	onBoundary func()

	// onBlock, if set, is called with each deflate block's BTYPE (0 stored,
	// 1 fixed, 2 dynamic) right after it is read. Used only by tests.
	onBlock func(btype int)
}

// emit outputs one decoded byte: into the window and the running CRC/length.
func (inf *inflater) emit(b byte) {
	inf.win.put(b)
	inf.crc = crcTab[byte(inf.crc)^b] ^ (inf.crc >> 8)
	inf.total++
	inf.memberLen++
}

// crcTab is the IEEE CRC32 lookup table (value copy for allocation-free indexing).
var crcTab = *crc32.MakeTable(crc32.IEEE)

// run decompresses the whole (possibly multi-member) gzip stream, firing
// onBoundary at each block boundary. It validates each member's CRC32 and
// ISIZE. It does not retain decompressed output beyond the sliding window.
func (inf *inflater) run() error {
	for {
		if err := inf.readGzipHeader(); err != nil {
			return err
		}
		inf.crc = 0xffffffff
		inf.memberLen = 0
		inf.memberStart = inf.total
		if err := inf.inflateBlocks(); err != nil {
			return err
		}
		if err := inf.readGzipTrailer(); err != nil {
			return err
		}
		if inf.br.eof() {
			return nil
		}
		// Otherwise a concatenated member follows (e.g. pigz/mgzip output).
	}
}

// readGzipHeader consumes one RFC 1952 gzip header.
func (inf *inflater) readGzipHeader() error {
	id1, err := inf.br.readByte()
	if err != nil {
		return err
	}
	id2, err := inf.br.readByte()
	if err != nil {
		return err
	}
	if id1 != 0x1f || id2 != 0x8b {
		return fmt.Errorf("ztoc: invalid gzip magic %#02x %#02x", id1, id2)
	}
	cm, err := inf.br.readByte()
	if err != nil {
		return err
	}
	if cm != 8 {
		return fmt.Errorf("ztoc: unsupported gzip compression method %d", cm)
	}
	flg, err := inf.br.readByte()
	if err != nil {
		return err
	}
	// MTIME(4) + XFL(1) + OS(1).
	for i := 0; i < 6; i++ {
		if _, err := inf.br.readByte(); err != nil {
			return err
		}
	}
	if flg&flagExtra != 0 {
		lo, err := inf.br.readByte()
		if err != nil {
			return err
		}
		hi, err := inf.br.readByte()
		if err != nil {
			return err
		}
		xlen := int(lo) | int(hi)<<8
		for i := 0; i < xlen; i++ {
			if _, err := inf.br.readByte(); err != nil {
				return err
			}
		}
	}
	if flg&flagName != 0 {
		if err := inf.skipCString(); err != nil {
			return err
		}
	}
	if flg&flagComment != 0 {
		if err := inf.skipCString(); err != nil {
			return err
		}
	}
	if flg&flagHCRC != 0 {
		for i := 0; i < 2; i++ {
			if _, err := inf.br.readByte(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (inf *inflater) skipCString() error {
	for {
		b, err := inf.br.readByte()
		if err != nil {
			return err
		}
		if b == 0 {
			return nil
		}
	}
}

// readGzipTrailer consumes and validates the 8-byte CRC32 + ISIZE trailer.
func (inf *inflater) readGzipTrailer() error {
	inf.br.align()
	var crc, isize uint32
	for i := 0; i < 4; i++ {
		b, err := inf.br.readByte()
		if err != nil {
			return err
		}
		crc |= uint32(b) << (8 * i)
	}
	for i := 0; i < 4; i++ {
		b, err := inf.br.readByte()
		if err != nil {
			return err
		}
		isize |= uint32(b) << (8 * i)
	}
	if crc != ^inf.crc {
		return fmt.Errorf("ztoc: gzip CRC mismatch: got %#08x want %#08x", ^inf.crc, crc)
	}
	if isize != inf.memberLen {
		return fmt.Errorf("ztoc: gzip ISIZE mismatch: got %d want %d", inf.memberLen, isize)
	}
	return nil
}

// inflateBlocks decodes deflate blocks until (and including) the final block.
func (inf *inflater) inflateBlocks() error {
	for {
		// A block boundary: report it before reading the next block header.
		if inf.onBoundary != nil {
			inf.onBoundary()
		}
		last, err := inf.br.bits(1)
		if err != nil {
			return err
		}
		btype, err := inf.br.bits(2)
		if err != nil {
			return err
		}
		if inf.onBlock != nil {
			inf.onBlock(btype)
		}
		switch btype {
		case 0:
			err = inf.stored()
		case 1:
			err = inf.codes(inf.fixedTables())
		case 2:
			var lc, dc *huffman
			lc, dc, err = inf.dynamicTables()
			if err == nil {
				err = inf.codes2(lc, dc)
			}
		default:
			err = errors.New("ztoc: invalid deflate block type 3")
		}
		if err != nil {
			return err
		}
		if last == 1 {
			return nil
		}
	}
}

// stored copies a non-compressed (stored) block.
func (inf *inflater) stored() error {
	inf.br.align()
	lo, err := inf.br.readByte()
	if err != nil {
		return err
	}
	hi, err := inf.br.readByte()
	if err != nil {
		return err
	}
	length := int(lo) | int(hi)<<8
	nlo, err := inf.br.readByte()
	if err != nil {
		return err
	}
	nhi, err := inf.br.readByte()
	if err != nil {
		return err
	}
	nlen := int(nlo) | int(nhi)<<8
	if length != (^nlen & 0xffff) {
		return errors.New("ztoc: stored block length check failed")
	}
	for i := 0; i < length; i++ {
		b, err := inf.br.readByte()
		if err != nil {
			return err
		}
		inf.emit(b)
	}
	return nil
}

// fixedTables returns the (lazily built, reused) fixed Huffman tables.
func (inf *inflater) fixedTables() *huffman {
	if inf.fixedLen == nil {
		var lengths [fixLCodes]int
		for i := 0; i < 144; i++ {
			lengths[i] = 8
		}
		for i := 144; i < 256; i++ {
			lengths[i] = 9
		}
		for i := 256; i < 280; i++ {
			lengths[i] = 7
		}
		for i := 280; i < fixLCodes; i++ {
			lengths[i] = 8
		}
		inf.fixedLen = &huffman{}
		construct(inf.fixedLen, lengths[:])

		var dlengths [maxDCodes]int
		for i := range dlengths {
			dlengths[i] = 5
		}
		inf.fixedDist = &huffman{}
		construct(inf.fixedDist, dlengths[:]) // intentionally incomplete (30 of 32)
	}
	return inf.fixedLen
}

// codes decodes a fixed-Huffman block (distance table taken from fixedDist).
func (inf *inflater) codes(lc *huffman) error {
	return inf.codes2(lc, inf.fixedDist)
}

// dynamicTables reads a dynamic block's Huffman code descriptions and returns
// the literal/length and distance decode tables.
func (inf *inflater) dynamicTables() (*huffman, *huffman, error) {
	hlit, err := inf.br.bits(5)
	if err != nil {
		return nil, nil, err
	}
	hlit += 257
	hdist, err := inf.br.bits(5)
	if err != nil {
		return nil, nil, err
	}
	hdist++
	hclen, err := inf.br.bits(4)
	if err != nil {
		return nil, nil, err
	}
	hclen += 4
	if hlit > maxLCodes || hdist > maxDCodes {
		return nil, nil, errors.New("ztoc: too many length or distance codes")
	}

	var clLengths [19]int
	for i := 0; i < hclen; i++ {
		v, err := inf.br.bits(3)
		if err != nil {
			return nil, nil, err
		}
		clLengths[codeLengthOrder[i]] = v
	}
	var clCode huffman
	if construct(&clCode, clLengths[:]) != 0 {
		return nil, nil, errors.New("ztoc: incomplete code-length code")
	}

	// Read hlit+hdist code lengths using the code-length code.
	lengths := make([]int, hlit+hdist)
	for i := 0; i < len(lengths); {
		sym, err := inf.decode(&clCode)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case sym < 16:
			lengths[i] = sym
			i++
		case sym == 16: // repeat previous length 3..6 times
			if i == 0 {
				return nil, nil, errors.New("ztoc: repeat with no previous length")
			}
			n, err := inf.br.bits(2)
			if err != nil {
				return nil, nil, err
			}
			n += 3
			if i+n > len(lengths) {
				return nil, nil, errors.New("ztoc: code-length repeat overruns")
			}
			prev := lengths[i-1]
			for ; n > 0; n-- {
				lengths[i] = prev
				i++
			}
		case sym == 17: // repeat zero 3..10 times
			n, err := inf.br.bits(3)
			if err != nil {
				return nil, nil, err
			}
			n += 3
			if i+n > len(lengths) {
				return nil, nil, errors.New("ztoc: code-length repeat overruns")
			}
			for ; n > 0; n-- {
				lengths[i] = 0
				i++
			}
		default: // sym == 18: repeat zero 11..138 times
			n, err := inf.br.bits(7)
			if err != nil {
				return nil, nil, err
			}
			n += 11
			if i+n > len(lengths) {
				return nil, nil, errors.New("ztoc: code-length repeat overruns")
			}
			for ; n > 0; n-- {
				lengths[i] = 0
				i++
			}
		}
	}
	if lengths[256] == 0 {
		return nil, nil, errors.New("ztoc: missing end-of-block code")
	}

	// An incomplete literal/length or distance code is tolerated only in the
	// degenerate single-code case, matching zlib/puff.c; otherwise reject.
	litLengths := lengths[:hlit]
	if r := construct(&inf.dynLen, litLengths); r != 0 && (r < 0 || countNonzero(litLengths) != 1) {
		return nil, nil, errors.New("ztoc: invalid literal/length code")
	}
	distLengths := lengths[hlit:]
	if r := construct(&inf.dynDist, distLengths); r != 0 && (r < 0 || countNonzero(distLengths) != 1) {
		return nil, nil, errors.New("ztoc: invalid distance code")
	}
	return &inf.dynLen, &inf.dynDist, nil
}

func countNonzero(lengths []int) int {
	n := 0
	for _, l := range lengths {
		if l != 0 {
			n++
		}
	}
	return n
}

// codes2 decodes literal/length + distance symbols for one block.
func (inf *inflater) codes2(lc, dc *huffman) error {
	for {
		sym, err := inf.decode(lc)
		if err != nil {
			return err
		}
		switch {
		case sym == endOfBlock:
			return nil
		case sym < endOfBlock:
			inf.emit(byte(sym))
		default:
			sym -= 257
			if sym >= len(lengthBase) {
				return errors.New("ztoc: invalid length symbol")
			}
			extra, err := inf.br.bits(uint(lengthExtra[sym]))
			if err != nil {
				return err
			}
			length := lengthBase[sym] + extra

			dsym, err := inf.decode(dc)
			if err != nil {
				return err
			}
			if dsym >= len(distBase) {
				return errors.New("ztoc: invalid distance symbol")
			}
			dextra, err := inf.br.bits(uint(distExtra[dsym]))
			if err != nil {
				return err
			}
			dist := distBase[dsym] + dextra
			if int64(dist) > inf.total-inf.memberStart {
				return errors.New("ztoc: distance back-reference before start of gzip member")
			}
			for ; length > 0; length-- {
				inf.emit(inf.win.byteAt(dist))
			}
		}
	}
}
