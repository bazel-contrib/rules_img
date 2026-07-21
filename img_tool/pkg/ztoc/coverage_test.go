package ztoc

import (
	"testing"
)

// TestCorpusBlockTypeCoverage confirms the corpus exercises all three DEFLATE
// block types: stored (0), fixed-Huffman (1), and dynamic-Huffman (2). If this
// fails because a type is missing, add an input that produces it rather than
// weakening the assertion.
func TestCorpusBlockTypeCoverage(t *testing.T) {
	seen := map[int]bool{}
	for _, e := range loadCorpus(t) {
		data := corpusFile(t, e.Input)
		inf := &inflater{}
		inf.br.in = data
		inf.onBlock = func(btype int) { seen[btype] = true }
		if err := inf.run(); err != nil {
			t.Fatalf("%s: inflate: %v", e.Name, err)
		}
	}
	for btype, name := range map[int]string{0: "stored", 1: "fixed", 2: "dynamic"} {
		if !seen[btype] {
			t.Errorf("no %s (btype=%d) deflate block found anywhere in the corpus", name, btype)
		}
	}
	t.Logf("block types seen across corpus: stored=%v fixed=%v dynamic=%v", seen[0], seen[1], seen[2])
}
