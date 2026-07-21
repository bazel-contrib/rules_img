//go:build ignore

// Command gen produces the ztoc test-vector corpus: it writes a set of
// deterministic .tar.gz inputs that exercise a wide range of gzip and tar
// features, then invokes the soci-snapshotter oracle (the authoritative,
// cgo/zlib implementation) to produce a golden .ztoc for each (input, span)
// pair. The inputs and goldens are committed under img_tool/pkg/ztoc/testdata;
// the Go tests there rebuild each ztoc in pure Go and require byte equality.
//
// Inputs are fully deterministic (fixed mtimes, seeded PRNG, no host-dependent
// fields) so the corpus is reproducible.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

var (
	outDir     string
	oracleBin  string
	epoch      = time.Unix(1700000000, 0).UTC() // fixed mtime
	epoch2     = time.Unix(1234567890, 123456789).UTC()
)

type vector struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Span  int64  `json:"span"`
}

var manifest []vector

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: gen <outDir> <oracleBinary>")
		os.Exit(2)
	}
	outDir = os.Args[1]
	oracleBin = os.Args[2]
	must(os.MkdirAll(outDir, 0o755))

	// --- Build the base tar payloads. ---

	emptyTar := buildTar(nil)

	singleTar := buildTar([]entry{
		{h: &tar.Header{Name: "hello.txt", Mode: 0o644, Size: 13, ModTime: epoch, Typeflag: tar.TypeReg, Uid: 0, Gid: 0}, body: []byte("hello, world\n")},
	})

	basicTar := buildTar([]entry{
		{h: dir("etc/")},
		{h: reg("etc/hostname", 0o644, 9, epoch), body: []byte("localhost")},
		{h: dir("usr/")},
		{h: dir("usr/bin/")},
		{h: reg("usr/bin/true", 0o755, 32, epoch), body: bytes.Repeat([]byte("A"), 32)},
		{h: reg("empty", 0o600, 0, epoch), body: nil},
	})

	// Rich metadata: symlink, hardlink, devices, fifo, xattrs, long name,
	// long linkname, setuid/sticky, sub-second mtime, non-zero uid/gid/names.
	metaTar := buildTar([]entry{
		{h: dir("d/")},
		{h: &tar.Header{Name: "d/regular", Mode: 0o4755, Size: 5, ModTime: epoch2, Typeflag: tar.TypeReg, Uid: 1000, Gid: 1000, Uname: "alice", Gname: "staff"}, body: []byte("data\n")},
		{h: &tar.Header{Name: "d/sticky", Mode: 0o1777, ModTime: epoch, Typeflag: tar.TypeDir}},
		{h: &tar.Header{Name: "d/link-symbolic", Mode: 0o777, ModTime: epoch, Typeflag: tar.TypeSymlink, Linkname: "regular"}},
		{h: &tar.Header{Name: "d/link-hard", Mode: 0o644, ModTime: epoch, Typeflag: tar.TypeLink, Linkname: "d/regular"}},
		{h: &tar.Header{Name: "d/chardev", Mode: 0o644, ModTime: epoch, Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}},
		{h: &tar.Header{Name: "d/blockdev", Mode: 0o644, ModTime: epoch, Typeflag: tar.TypeBlock, Devmajor: 8, Devminor: 0}},
		{h: &tar.Header{Name: "d/fifo", Mode: 0o644, ModTime: epoch, Typeflag: tar.TypeFifo}},
		{h: &tar.Header{Name: "d/withxattrs", Mode: 0o644, Size: 3, ModTime: epoch, Typeflag: tar.TypeReg, PAXRecords: map[string]string{"SCHILY.xattr.user.comment": "hi", "SCHILY.xattr.security.selinux": "unconfined_u"}}, body: []byte("xa\n")},
		{h: &tar.Header{Name: "d/" + longName(150), Mode: 0o644, Size: 2, ModTime: epoch, Typeflag: tar.TypeReg}, body: []byte("ln")},
		{h: &tar.Header{Name: "d/longlink", Mode: 0o777, ModTime: epoch, Typeflag: tar.TypeSymlink, Linkname: longName(200)}},
	})

	manyTar := func() []byte {
		var es []entry
		es = append(es, entry{h: dir("many/")})
		for i := 0; i < 120; i++ {
			name := fmt.Sprintf("many/file-%04d.txt", i)
			body := []byte(fmt.Sprintf("this is file number %d with some repeated content %s\n", i, longName(20)))
			es = append(es, entry{h: reg(name, 0o644, int64(len(body)), epoch), body: body})
		}
		return buildTar(es)
	}()

	// ~8 MiB of compressible data across a few files -> crosses a 4 MiB span.
	largeCompressible := func() []byte {
		var es []entry
		for f := 0; f < 4; f++ {
			body := bytes.Repeat([]byte(fmt.Sprintf("line %d: the quick brown fox jumps over the lazy dog 0123456789\n", f)), 40000)
			es = append(es, entry{h: reg(fmt.Sprintf("big/part-%d.log", f), 0o644, int64(len(body)), epoch), body: body})
		}
		return buildTar(es)
	}()

	// ~128 KiB of seeded-random (incompressible) data -> stored/low-ratio blocks.
	incompressible := func() []byte {
		rng := rand.New(rand.NewSource(42))
		body := make([]byte, 131072)
		rng.Read(body)
		return buildTar([]entry{{h: reg("random.bin", 0o644, int64(len(body)), epoch), body: body}})
	}()

	// ~64 KiB of lorem-ish text -> dynamic Huffman across many blocks.
	textHeavy := func() []byte {
		var buf bytes.Buffer
		words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
		rng := rand.New(rand.NewSource(7))
		for buf.Len() < 65536 {
			buf.WriteString(words[rng.Intn(len(words))])
			buf.WriteByte(' ')
			if rng.Intn(12) == 0 {
				buf.WriteByte('\n')
			}
		}
		return buildTar([]entry{{h: reg("lorem.txt", 0o644, int64(buf.Len()), epoch), body: buf.Bytes()}})
	}()

	// --- Emit vectors: (payload, gzip variant) x span sizes. ---
	//
	// Golden size is ~32 KiB per checkpoint, so large inputs are only paired
	// with large spans (few checkpoints) and small inputs carry the
	// many-checkpoint coverage.

	// Compression levels (incl. 0 = stored blocks) on the small "basic" payload.
	for _, lv := range []int{gzip.NoCompression, gzip.BestSpeed, gzip.DefaultCompression, gzip.BestCompression} {
		emit(fmt.Sprintf("basic-l%d", lv), gzipLevel(basicTar, lv), 4096)
	}

	emit("empty", gzipLevel(emptyTar, gzip.DefaultCompression), 4096)
	emit("single", gzipLevel(singleTar, gzip.DefaultCompression), 4096)
	emit("meta", gzipLevel(metaTar, gzip.DefaultCompression), 4096)

	// "many" small files: several checkpoints at a small span, fewer at a larger.
	emit("many-l0", gzipLevel(manyTar, gzip.NoCompression), 4096)
	emit("many-l6", gzipLevel(manyTar, gzip.DefaultCompression), 4096, 1<<14)
	emit("many-l9", gzipLevel(manyTar, gzip.BestCompression), 1<<14)

	// text payloads: dynamic Huffman across many blocks, one high-checkpoint case.
	emit("text-l6", gzipLevel(textHeavy, gzip.DefaultCompression), 4096, 1<<16)
	emit("text-l0", gzipLevel(textHeavy, gzip.NoCompression), 1<<16)
	emit("text-l9", gzipLevel(textHeavy, gzip.BestCompression), 1<<16)

	// Large payload crossing a realistic 4 MiB span (2 spans, small golden).
	emit("large-compressible-l1", gzipLevel(largeCompressible, gzip.BestSpeed), 1<<22)
	emit("large-compressible-l6", gzipLevel(largeCompressible, gzip.DefaultCompression), 1<<22)

	// Incompressible data: stored blocks with multiple checkpoints.
	emit("incompressible-l0", gzipLevel(incompressible, gzip.NoCompression), 1<<14, 1<<16)
	emit("incompressible-l9", gzipLevel(incompressible, gzip.BestCompression), 1<<16)

	// Gzip header variations (FNAME/FCOMMENT/FEXTRA/MTIME) -> header length varies,
	// which shifts checkpoint 0's compressed offset.
	emit("hdr-name", gzipWithHeader(basicTar, gzip.Header{Name: "basic.tar", ModTime: epoch}), 4096)
	emit("hdr-comment", gzipWithHeader(basicTar, gzip.Header{Comment: "a comment", ModTime: epoch}), 4096)
	emit("hdr-extra", gzipWithHeader(basicTar, gzip.Header{Extra: []byte("AB\x04\x00wxyz"), ModTime: epoch}), 4096)
	emit("hdr-all", gzipWithHeader(basicTar, gzip.Header{Name: "n", Comment: "c", Extra: []byte("XY\x02\x00hi"), ModTime: epoch, OS: 3}), 4096)

	// Multi-member (concatenated) gzip streams (pigz/mgzip style).
	emit("multi-basic", gzipMultistream(basicTar, 3), 4096)
	emit("multi-text", gzipMultistream(textHeavy, 4), 1<<16)
	emit("multi-large", gzipMultistream(largeCompressible, 5), 1<<22)

	// Write the manifest.
	sort.Slice(manifest, func(i, j int) bool {
		if manifest[i].Name != manifest[j].Name {
			return manifest[i].Name < manifest[j].Name
		}
		return manifest[i].Span < manifest[j].Span
	})
	mb, _ := json.MarshalIndent(manifest, "", "  ")
	must(os.WriteFile(filepath.Join(outDir, "manifest.json"), append(mb, '\n'), 0o644))
	fmt.Printf("generated %d vectors\n", len(manifest))
}

// emit writes the gzip input for name and, for each span, produces a golden.
func emit(name string, gz []byte, spans ...int64) {
	inputFile := name + ".tar.gz"
	must(os.WriteFile(filepath.Join(outDir, inputFile), gz, 0o644))
	for _, span := range spans {
		vecName := name
		if len(spans) > 1 {
			vecName = fmt.Sprintf("%s-s%d", name, span)
		}
		golden := vecName + ".ztoc"
		cmd := exec.Command(oracleBin, filepath.Join(outDir, inputFile), strconv.FormatInt(span, 10), filepath.Join(outDir, golden))
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			panic(fmt.Sprintf("oracle failed for %s span=%d: %v", name, span, err))
		}
		manifest = append(manifest, vector{Name: vecName, Input: inputFile, Span: span})
	}
}

type entry struct {
	h    *tar.Header
	body []byte
}

func buildTar(entries []entry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if e.h.Size == 0 && len(e.body) > 0 {
			e.h.Size = int64(len(e.body))
		}
		must(tw.WriteHeader(e.h))
		if len(e.body) > 0 {
			_, err := tw.Write(e.body)
			must(err)
		}
	}
	must(tw.Close())
	return buf.Bytes()
}

func dir(name string) *tar.Header {
	return &tar.Header{Name: name, Mode: 0o755, ModTime: epoch, Typeflag: tar.TypeDir}
}

func reg(name string, mode int64, size int64, mt time.Time) *tar.Header {
	return &tar.Header{Name: name, Mode: mode, Size: size, ModTime: mt, Typeflag: tar.TypeReg}
}

func longName(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return string(b)
}

func gzipLevel(data []byte, level int) []byte {
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, level)
	must(err)
	w.ModTime = epoch // deterministic header
	_, err = w.Write(data)
	must(err)
	must(w.Close())
	return buf.Bytes()
}

func gzipWithHeader(data []byte, hdr gzip.Header) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Header = hdr
	_, err := w.Write(data)
	must(err)
	must(w.Close())
	return buf.Bytes()
}

// gzipMultistream compresses data as `members` separately-gzipped chunks
// concatenated together; decompressing the concatenation yields data.
func gzipMultistream(data []byte, members int) []byte {
	var out bytes.Buffer
	n := len(data)
	for i := 0; i < members; i++ {
		start := i * n / members
		end := (i + 1) * n / members
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		w.ModTime = epoch
		_, err := w.Write(data[start:end])
		must(err)
		must(w.Close())
		out.Write(buf.Bytes())
	}
	return out.Bytes()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}