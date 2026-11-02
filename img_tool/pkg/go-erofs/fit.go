package erofs

// fitSearch finds the longest prefix of an input whose compressed form fits a
// budget, for the compressors that cannot be told to stop when the output is
// full. It is a search: the input is compressed whole at candidate lengths.
type fitSearch struct {
	tmp  []byte
	best []byte
}

// searchGranularity is how closely the search homes in on the largest input that
// fits, in bytes at least, and a 32nd of the input at most: each probe compresses
// the whole candidate.
const searchGranularity = 32

// search returns the compressed form of the longest prefix of src that it found
// to fit budget bytes, and the length of that prefix. compress appends the
// compressed form of its input to dst, and reports false for an input whose
// result cannot be used.
func (s *fitSearch) search(src []byte, budget int, compress func(dst, src []byte) ([]byte, bool)) ([]byte, int) {
	fits := func(n int) bool {
		var ok bool
		s.tmp, ok = compress(s.tmp[:0], src[:n])
		return ok && len(s.tmp) <= budget
	}
	keep := func() { s.best = append(s.best[:0], s.tmp...) }

	// Grow geometrically until the output no longer fits, then narrow down.
	lo, hi := 0, 0
	for n := min(budget, len(src)); ; n = min(n*2, len(src)) {
		if !fits(n) {
			hi = n
			break
		}
		lo = n
		keep()
		if n == len(src) {
			return s.best, n
		}
	}
	for hi-lo > max(searchGranularity, lo/32) {
		mid := lo + (hi-lo)/2
		if fits(mid) {
			lo = mid
			keep()
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return nil, 0
	}
	return s.best, lo
}
