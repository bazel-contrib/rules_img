package erofs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// lz4DecodeStrict decodes a bare LZ4 block and enforces the end-of-block rules
// the kernel's decoder relies on: the block ends with a literal-only sequence
// and its last match starts at least 12 bytes before the end of the output.
func lz4DecodeStrict(in []byte, want int) ([]byte, error) {
	out := make([]byte, 0, want)
	lastMatchStart := -1
	for i := 0; ; {
		if i >= len(in) {
			return nil, fmt.Errorf("block ends without a final literal sequence")
		}
		tok := in[i]
		i++
		lits := int(tok >> 4)
		if lits == 15 {
			for {
				if i >= len(in) {
					return nil, fmt.Errorf("truncated literal length")
				}
				b := in[i]
				i++
				lits += int(b)
				if b != 255 {
					break
				}
			}
		}
		if i+lits > len(in) {
			return nil, fmt.Errorf("literals overrun the block")
		}
		out = append(out, in[i:i+lits]...)
		i += lits
		if i == len(in) {
			if len(out) < lz4LastLiterals && lastMatchStart >= 0 {
				return nil, fmt.Errorf("fewer than %d trailing literals", lz4LastLiterals)
			}
			if lastMatchStart >= 0 && len(out)-lastMatchStart < lz4MFLimit {
				return nil, fmt.Errorf("last match starts %d bytes from the end", len(out)-lastMatchStart)
			}
			return out, nil
		}
		if i+2 > len(in) {
			return nil, fmt.Errorf("truncated offset")
		}
		off := int(binary.LittleEndian.Uint16(in[i:]))
		i += 2
		mlen := int(tok & 15)
		if mlen == 15 {
			for {
				if i >= len(in) {
					return nil, fmt.Errorf("truncated match length")
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
		if off == 0 || off > len(out) {
			return nil, fmt.Errorf("bad offset %d at output %d", off, len(out))
		}
		lastMatchStart = len(out)
		for j := 0; j < mlen; j++ {
			out = append(out, out[len(out)-off])
		}
	}
}

func testPatterns(rng *rand.Rand) map[string][]byte {
	random := make([]byte, 300<<10)
	rng.Read(random)
	text := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 20000)
	zeros := make([]byte, 600<<10)
	mixed := make([]byte, 0, 500<<10)
	for len(mixed) < 500<<10 {
		switch rng.Intn(3) {
		case 0:
			mixed = append(mixed, random[:rng.Intn(9000)]...)
		case 1:
			mixed = append(mixed, text[:rng.Intn(9000)]...)
		default:
			mixed = append(mixed, zeros[:rng.Intn(9000)]...)
		}
	}
	return map[string][]byte{"random": random, "text": text, "zeros": zeros, "mixed": mixed}
}

func TestLZ4EncodeFits(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	var enc lz4Encoder
	for name, data := range testPatterns(rng) {
		for _, budget := range []int{512, 4096} {
			for _, start := range []int{0, 1, 4095, 4096, 12345, len(data) - 3000, len(data) - 50, len(data) - 5} {
				if start < 0 || start >= len(data) {
					continue
				}
				src := data[start:]
				final := len(src) <= zMaxExtent
				if !final {
					src = src[:zMaxExtent]
				}
				out, n := enc.encode(src, budget, final)
				if n == 0 {
					continue
				}
				if len(out) > budget {
					t.Fatalf("%s budget %d start %d: block is %d bytes", name, budget, start, len(out))
				}
				got, err := lz4DecodeStrict(out, n)
				if err != nil {
					t.Fatalf("%s budget %d start %d: %v", name, budget, start, err)
				}
				if !bytes.Equal(got, src[:n]) {
					t.Fatalf("%s budget %d start %d: round trip mismatch (n=%d)", name, budget, start, n)
				}
			}
		}
	}
}

// decodeEncodedFile is a reference reader for an encoded file. It follows the
// kernel's lookup rules (fs/erofs/zmap.c): it checks the NONHEAD distances
// against the heads they refer to, and decodes every extent.
func decodeEncodedFile(t *testing.T, z *EncodedFile, blocks []byte, size int64) []byte {
	t.Helper()
	bs := z.blockSize
	meta := z.appendMeta(nil, 0, 1000) // base 1000 to catch unresolved block numbers
	if len(meta) != z.metaSize(0) {
		t.Fatalf("metaSize %d != serialized %d", z.metaSize(0), len(meta))
	}
	idx := meta[zMapHeaderSize:]
	n := len(idx) / zIndexSize
	type lc struct {
		typ        int
		clusterofs int
		u          uint32
	}
	lcs := make([]lc, n)
	for i := range lcs {
		b := idx[i*zIndexSize:]
		lcs[i] = lc{int(binary.LittleEndian.Uint16(b) & 3), int(binary.LittleEndian.Uint16(b[2:])), binary.LittleEndian.Uint32(b[4:])}
	}

	// Collect extents: [start, end) and the head that describes them.
	type ext struct {
		start, end int64
		head       int
	}
	var exts []ext
	for i, l := range lcs {
		if l.typ == zTypeNonHead {
			continue
		}
		exts = append(exts, ext{start: int64(i*bs + l.clusterofs), head: i})
	}
	for i := range exts {
		if i+1 < len(exts) {
			exts[i].end = exts[i+1].start
		} else {
			exts[i].end = size
		}
	}
	// Consistency of NONHEAD deltas with the heads (kernel lookback/lookahead).
	for i, l := range lcs {
		if l.typ != zTypeNonHead {
			continue
		}
		d0, d1 := int(zLcluster{typ: l.typ, u: l.u}.delta0()), int(l.u>>16)
		if d0 == 0 || d0 > i {
			t.Fatalf("lcn %d: bad delta0 %d", i, d0)
		}
		// Walk back as the kernel does.
		j := i - d0
		for lcs[j].typ == zTypeNonHead {
			dd := int(zLcluster{typ: lcs[j].typ, u: lcs[j].u}.delta0())
			if dd == 0 || dd > j {
				t.Fatalf("lcn %d: bad lookback chain at %d", i, j)
			}
			j -= dd
		}
		// j is the head; the previous head must not lie between j and i.
		for k := j + 1; k < i; k++ {
			if lcs[k].typ != zTypeNonHead {
				t.Fatalf("lcn %d: lookback skips head %d", i, k)
			}
		}
		if i+d1 < n {
			if lcs[i+d1].typ == zTypeNonHead {
				t.Fatalf("lcn %d: delta1 %d points at a NONHEAD", i, d1)
			}
			for k := i + 1; k < i+d1; k++ {
				if lcs[k].typ != zTypeNonHead {
					t.Fatalf("lcn %d: delta1 skips head %d", i, k)
				}
			}
		}
	}

	out := make([]byte, 0, size)
	for _, e := range exts {
		if e.start == e.end && e.start == size {
			continue // terminating marker
		}
		if e.start != int64(len(out)) {
			t.Fatalf("extent at %d, expected %d", e.start, len(out))
		}
		l := lcs[e.head]
		blk := int(l.u) - 1000
		plen := 1
		if l.typ == zTypeHead1 && e.head+1 < n && lcs[e.head+1].typ == zTypeNonHead && lcs[e.head+1].u&zCBlkCnt != 0 {
			plen = int(lcs[e.head+1].u & 0xffff &^ zCBlkCnt)
		}
		if !z.big && plen != 1 {
			t.Fatalf("lcn %d: cluster of %d blocks without big clusters", e.head, plen)
		}
		if blk < 0 || (blk+plen)*bs > len(blocks) {
			t.Fatalf("lcn %d: blocks %d+%d out of range", e.head, blk, plen)
		}
		cluster := blocks[blk*bs : (blk+plen)*bs]
		length := int(e.end - e.start)
		switch l.typ {
		case zTypePlain:
			if length > bs {
				t.Fatalf("plain extent of %d bytes in a %d-byte block", length, bs)
			}
			out = append(out, cluster[:length]...)
		case zTypeHead1:
			if z.alg == CompressionZstd || z.alg == CompressionDeflate {
				dec := make([]byte, length)
				skip := 0
				for skip < len(cluster) && cluster[skip] == 0 {
					skip++
				}
				decode := zstdDecode
				if z.alg == CompressionDeflate {
					decode = deflateDecode
				}
				if err := decode(dec, cluster[skip:]); err != nil {
					t.Fatalf("extent at %d: %v", e.start, err)
				}
				out = append(out, dec...)
				break
			}
			skip := 0
			for skip < len(cluster) && cluster[skip] == 0 {
				skip++
			}
			dec, err := lz4DecodeStrict(cluster[skip:], length)
			if err != nil {
				t.Fatalf("extent at %d: %v", e.start, err)
			}
			if len(dec) != length {
				t.Fatalf("extent at %d decodes to %d bytes, want %d", e.start, len(dec), length)
			}
			out = append(out, dec...)
		}
		// A non-last extent must reach the end of its first lcluster.
		if e.end != size && e.start/int64(bs) == e.end/int64(bs) && !(e.end%int64(bs) == 0) {
			t.Fatalf("extent [%d,%d) lies within one lcluster", e.start, e.end)
		}
	}
	return out
}

func TestEncodeFileRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	patterns := testPatterns(rng)
	for _, alg := range []CompressionAlgorithm{CompressionLZ4, CompressionDeflate, CompressionZstd} {
		for _, cb := range []int{1, 4, 16} {
			for _, bs := range []int{512, 4096} {
				for name, data := range patterns {
					sizes := []int{bs + 1, 2 * bs, 3*bs - 1, 5000, 8191, 8192, 8193, 100000, len(data)}
					for _, size := range sizes {
						if size > len(data) {
							continue
						}
						t.Run(fmt.Sprintf("%v/c%d/%s/bs%d/%d", alg, cb, name, bs, size), func(t *testing.T) {
							src := data[:size]
							var blocks bytes.Buffer
							z, err := EncodeFileClusters(&blocks, bytes.NewReader(src), int64(size), bs, cb, alg)
							if err != nil {
								t.Fatal(err)
							}
							if blocks.Len() != z.Blocks*bs {
								t.Fatalf("wrote %d bytes for %d blocks", blocks.Len(), z.Blocks)
							}
							if z.AllRaw {
								if !bytes.Equal(blocks.Bytes()[:size], src) {
									t.Fatal("AllRaw but the blocks are not the file")
								}
								return
							}
							got := decodeEncodedFile(t, z, blocks.Bytes(), int64(size))
							if !bytes.Equal(got, src) {
								t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), size)
							}
							if cb > 1 && z.AllRaw {
								return
							}
							if rawBlocks := (size + bs - 1) / bs; z.Blocks > rawBlocks {
								t.Fatalf("encoded %d blocks, more than the %d raw blocks", z.Blocks, rawBlocks)
							}

							// Determinism: encoding again gives identical bytes.
							var again bytes.Buffer
							z2, err := EncodeFileClusters(&again, bytes.NewReader(src), int64(size), bs, cb, alg)
							if err != nil {
								t.Fatal(err)
							}
							if !bytes.Equal(again.Bytes(), blocks.Bytes()) || len(z2.idx) != len(z.idx) {
								t.Fatal("encoding is not deterministic")
							}
						})
					}
				}
			}
		}
	}
}

func TestEncodeFileSavesSpace(t *testing.T) {
	text := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 5000)
	for _, alg := range []CompressionAlgorithm{CompressionLZ4, CompressionDeflate, CompressionZstd} {
		var blocks bytes.Buffer
		z, err := EncodeFile(&blocks, bytes.NewReader(text), int64(len(text)), 4096, alg)
		if err != nil {
			t.Fatal(err)
		}
		if z.AllRaw || z.Blocks*4 > (len(text)+4095)/4096 {
			t.Fatalf("%v: expected at least 4x compression of repeated text, got %d blocks for %d bytes", alg, z.Blocks, len(text))
		}
	}
}

func TestEncodeFileShortRead(t *testing.T) {
	var blocks bytes.Buffer
	_, err := EncodeFile(&blocks, bytes.NewReader(make([]byte, 5000)), 9000, 4096, CompressionLZ4)
	if err == nil {
		t.Fatal("expected an error for a reader that ends early")
	}
}

// TestWriteCompressedImage builds a compressed image and, when EROFS_Z_OUT names
// a directory, leaves the image and a sha256sum manifest of its files there so
// the kernel and erofs-utils can check it (see the commands in the log output).
func TestWriteCompressedImage(t *testing.T) {
	for _, alg := range []CompressionAlgorithm{CompressionLZ4, CompressionDeflate, CompressionZstd} {
		for _, cb := range []int{1, 4, 16} {
			name := fmt.Sprintf("%v-c%d", alg, cb)
			t.Run(name, func(t *testing.T) { writeCompressedImage(t, name, alg, cb) })
		}
	}
}

func writeCompressedImage(t *testing.T, name string, alg CompressionAlgorithm, clusterBlocks int) {
	t.Helper()
	rng := rand.New(rand.NewSource(3))
	patterns := testPatterns(rng)
	files := map[string][]byte{
		"/empty":            nil,
		"/tiny":             []byte("hello"),
		"/one-block":        patterns["text"][:4096],
		"/just-over":        patterns["text"][:4097],
		"/text/a":           patterns["text"][:100000],
		"/text/b":           patterns["text"],
		"/text/odd":         patterns["text"][:50001],
		"/random":           patterns["random"],
		"/random-small":     patterns["random"][:9000],
		"/zeros":            patterns["zeros"],
		"/mixed":            patterns["mixed"],
		"/nested/dir/mixed": patterns["mixed"][:123457],
		"/text/b-link":      patterns["text"],
	}
	w := NewWriter(WithCompression(alg), WithClusterBlocks(clusterBlocks), WithBlockSize(4096), WithBuildTime(0, 0))
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.HasSuffix(name, "-link") {
			continue
		}
		if err := w.AddFile(name, int64(len(files[name])), bytes.NewReader(files[name])); err != nil {
			t.Fatal(err)
		}
	}
	// Compressed inodes with extended attributes, a non-default owner and time
	// (which forces the 64-byte inode), and a hard link.
	for _, name := range []string{"/text/a", "/random", "/mixed"} {
		if err := w.Setxattr(name, "user.test", "value-of-"+name); err != nil {
			t.Fatal(err)
		}
		if err := w.Chown(name, 1000, 100); err != nil {
			t.Fatal(err)
		}
		if err := w.Chtimes(name, time.Unix(1700000000, 0), time.Unix(1700000000, 5)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Link("/text/b", "/text/b-link"); err != nil {
		t.Fatal(err)
	}
	layout, err := w.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	var img bytes.Buffer
	if _, err := w.WriteTo(&img); err != nil {
		t.Fatal(err)
	}
	if int64(img.Len()) != layout.ImageSize {
		t.Fatalf("wrote %d bytes, layout says %d", img.Len(), layout.ImageSize)
	}
	var compressed int
	for _, x := range layout.Extents {
		if x.Kind == ExtentCompressedFileData {
			compressed++
			if x.LogicalSize <= x.Size {
				t.Errorf("%s: compressed extent of %d bytes encodes only %d", x.Path, x.Size, x.LogicalSize)
			}
		}
	}
	if compressed == 0 {
		t.Fatal("no file was stored compressed")
	}
	t.Logf("image: %d bytes, %d compressed files", img.Len(), compressed)

	// The reader decodes what the writer produced.
	efs, err := Open(bytes.NewReader(img.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	extents := map[string]Extent{}
	for _, x := range layout.Extents {
		extents[x.Path] = x
	}
	for _, name := range names {
		got, err := fs.ReadFile(efs, name[1:])
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if !bytes.Equal(got, files[name]) {
			t.Fatalf("%s: read back %d bytes that differ from the %d written", name, len(got), len(files[name]))
		}
		info, err := efs.(fs.StatFS).Stat(name[1:])
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "-link") {
			continue // a hard link has no extent of its own
		}
		x, hasExtent := extents[name]
		rng, ok := info.(interface{ EncodedRange() (DataRange, bool) }).EncodedRange()
		if ok != (hasExtent && x.Kind == ExtentCompressedFileData) {
			t.Errorf("%s: EncodedRange reported %v for an extent of kind %v", name, ok, x.Kind)
		}
		if ok && (rng.Offset != x.Offset || rng.Size != x.Size) {
			t.Errorf("%s: EncodedRange = %+v, extent is at %d size %d", name, rng, x.Offset, x.Size)
		}
	}

	out := os.Getenv("EROFS_Z_OUT")
	if out == "" {
		return
	}
	out = filepath.Join(out, name)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "z.erofs"), img.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	var sums bytes.Buffer
	for _, name := range names {
		fmt.Fprintf(&sums, "%x  .%s\n", sha256.Sum256(files[name]), name)
	}
	if err := os.WriteFile(filepath.Join(out, "sums.txt"), sums.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// impliedBlocks counts the physical blocks the lcluster indexes of z account for,
// the way a reader derives them: a cluster is one block unless the lcluster after
// its head records more.
func impliedBlocks(z *EncodedFile) int {
	implied := 0
	for i, ix := range z.idx {
		if ix.advise == zTypeNonHead || ix.end {
			continue
		}
		p := 1
		if ix.advise == zTypeHead1 && i+1 < len(z.idx) && z.idx[i+1].advise == zTypeNonHead && z.idx[i+1].u&zCBlkCnt != 0 {
			p = int(z.idx[i+1].u & 0xffff &^ zCBlkCnt)
		}
		implied += p
	}
	return implied
}

// TestClusterBlocksAreRecorded is a regression test: the block count of a cluster
// of several blocks lives in the first NONHEAD lcluster of its extent, so an
// extent without one must not take more than a block. An incompressible tail
// that started in the middle of an lcluster and ended before the one after the
// next used to be written as a two-block cluster that readers took for one block.
func TestClusterBlocksAreRecorded(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	text := bytes.Repeat([]byte("compressible head of the file\n"), 4000)
	noise := make([]byte, 80000)
	rng.Read(noise)
	for _, alg := range []CompressionAlgorithm{CompressionLZ4, CompressionDeflate, CompressionZstd} {
		// A compressible head of every length shifts where the incompressible
		// tail starts within an lcluster.
		for head := 1; head < 30000; head += 211 {
			for tail := 4000; tail < 40000; tail += 313 {
				src := append(append([]byte(nil), text[:head]...), noise[:tail]...)
				var blocks bytes.Buffer
				z, err := EncodeFileClusters(&blocks, bytes.NewReader(src), int64(len(src)), 4096, 4, alg)
				if err != nil {
					t.Fatal(err)
				}
				if got := impliedBlocks(z); !z.AllRaw && got != z.Blocks {
					t.Fatalf("%v head %d tail %d: the indexes account for %d blocks, %d were written", alg, head, tail, got, z.Blocks)
				}
			}
		}
	}
}
