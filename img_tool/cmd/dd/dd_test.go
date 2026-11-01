package ddcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/casdirlayout"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/extentstream"
)

func TestParseNumber(t *testing.T) {
	for in, want := range map[string]int64{
		"0": 0, "7": 7, "512": 512, "1b": 512, "2w": 4, "3c": 3, "1K": 1024, "1KiB": 1024,
		"1kB": 1000, "4M": 4 << 20, "4MiB": 4 << 20, "2MB": 2000000, "1G": 1 << 30, "1GB": 1000000000,
	} {
		if got, err := parseNumber(in); err != nil || got != want {
			t.Errorf("parseNumber(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "M", "-1", "1x", "1.5M", "99999999999999999999", "9999999999999G"} {
		if _, err := parseNumber(in); err == nil {
			t.Errorf("parseNumber(%q) succeeded", in)
		}
	}
}

func TestParseArgs(t *testing.T) {
	o, err := parseArgs([]string{"if=a.img", "of=/dev/x", "bs=4M", "skip=2", "seek=3", "count=5", "status=progress", "conv=sparse,fsync", "iflag=skip_bytes", "oflag=seek_bytes", "cas-dir=d1", "--cas-dir", "d2"})
	if err != nil {
		t.Fatal(err)
	}
	if o.in != "a.img" || o.out != "/dev/x" || o.bs != 4<<20 || o.skip != 2 || o.seek != 3 || o.count != 5 || o.status != "progress" ||
		!o.sparse || !o.fsync || !o.skipBytes || !o.seekBytes || len(o.casDirs) != 2 {
		t.Errorf("%+v", o)
	}
	if o, err = parseArgs([]string{"--if=a", "--bs=512"}); err != nil || o.bs != 512 || o.count != -1 {
		t.Errorf("flag style: %+v, %v", o, err)
	}
	for _, bad := range [][]string{
		{}, {"if=a", "blob-descriptor=b"}, {"if=a", "bs=0"}, {"if=a", "bs=1T"}, {"if=a", "status=loud"}, {"if=a", "conv=magic"}, {"if=a", "conv=sparse,nosparse"},
		{"if=a", "iflag=nope"}, {"if=a", "oflag=nope"}, {"if=a", "stray"}, {"if=a", "count=x"},
		// A target built by image_dd passes the image; a second if= must not
		// silently replace it.
		{"if=a", "if=b"}, {"--if=a", "if=b"}, {"blob-descriptor=a", "blob-descriptor=b"},
	} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	// of= is an ordinary operand: the last one wins, as dd(1) does it.
	if o, err = parseArgs([]string{"if=a", "of=x", "of=y"}); err != nil || o.out != "y" {
		t.Errorf("repeated of=: %+v, %v", o, err)
	}
	if o, err = parseArgs([]string{"if=a", "cas-dir-manifest=m"}); err != nil || o.casDirManifest != "m" {
		t.Errorf("cas-dir-manifest: %+v, %v", o, err)
	}
}

func TestCASDirsFromManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cas_dirs.txt")
	if err := os.WriteFile(path, []byte("_main/a.cas\r\n\n_main/b.cas\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolve := func(p string) (string, error) { return "/runfiles/" + p, nil }
	got, err := casDirsFromManifest(path, resolve)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/runfiles/_main/a.cas", "/runfiles/_main/b.cas"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("casDirsFromManifest = %q; want %q", got, want)
	}
	// A directory that is not in the runfiles is an error, not a blob that is
	// silently missing later.
	if _, err := casDirsFromManifest(path, func(string) (string, error) { return "", fmt.Errorf("not found") }); err == nil {
		t.Error("an unresolvable runfiles path was accepted")
	}
	if _, err := casDirsFromManifest(filepath.Join(t.TempDir(), "gone.txt"), resolve); err == nil {
		t.Error("a missing manifest was accepted")
	}
}

// fixture is a disk-like image: data, a large hole, a CAS blob, more data.
type fixture struct {
	dir        string
	raw        string
	xstream    string
	cstream    string
	cas        string
	content    []byte
	blob       []byte
	holeLength int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), holeLength: 5 << 20}
	f.cas = filepath.Join(f.dir, "cas")
	f.blob = bytes.Repeat([]byte("blob-content-"), 100000) // ~1.3 MB
	sum := sha256.Sum256(f.blob)
	blobPath := casdirlayout.Path(filepath.Join(f.cas, "sha256"), hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath, f.blob, 0o644); err != nil {
		t.Fatal(err)
	}

	head, tail := []byte("HEAD-of-the-image"), []byte("tail")
	f.content = append(append(append(append([]byte{}, head...), make([]byte, f.holeLength)...), f.blob...), tail...)

	var xs bytes.Buffer
	w := extentstream.NewWriter(&xs)
	w.AddData(head)
	w.AddHole(int64(f.holeLength))
	w.AddRef(extentstream.KindCAS, sum[:], int64(len(f.blob)), 0, int64(len(f.blob)))
	w.AddData(tail)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.xstream = filepath.Join(f.dir, "disk.xstream")
	f.raw = filepath.Join(f.dir, "disk.img")
	for path, data := range map[string][]byte{f.xstream: xs.Bytes(), f.raw: f.content} {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The same content as a compact stream (the hole as inline zeros).
	var cs bytes.Buffer
	cw := compactstream.NewWriter(&cs, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionNone, compactstream.OriginalCompressionInfo{}, 1)
	cw.WriteStreamBytes(append(append([]byte{}, head...), make([]byte, f.holeLength)...))
	cw.WriteCASRef(sum[:], uint64(len(f.blob)))
	cw.WriteStreamBytes(tail)
	if err := cw.Close(); err != nil {
		t.Fatal(err)
	}
	f.cstream = filepath.Join(f.dir, "disk.cstream")
	if err := os.WriteFile(f.cstream, cs.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// dd runs img dd with the arguments and returns what it printed to stderr.
func (f *fixture) dd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	o, err := parseArgs(args)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	stdout, err := os.Create(filepath.Join(f.dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	err = run(context.Background(), o, stdout, &stderr)
	return stderr.String(), err
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInputsProduceTheSameBytes(t *testing.T) {
	f := newFixture(t)
	for name, in := range map[string]string{"raw": f.raw, "extent stream": f.xstream, "compact stream": f.cstream} {
		t.Run(name, func(t *testing.T) {
			out := filepath.Join(f.dir, "out-"+strings.ReplaceAll(name, " ", "-"))
			stderr, err := f.dd(t, "if="+in, "of="+out, "cas-dir="+f.cas)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readFile(t, out), f.content) {
				t.Error("the output differs from the image")
			}
			if !strings.Contains(stderr, "bytes") || !strings.Contains(stderr, "skipped") {
				t.Errorf("summary: %q", stderr)
			}
		})
	}
}

func TestStdoutGetsZerosForHoles(t *testing.T) {
	f := newFixture(t)
	if _, err := f.dd(t, "if="+f.xstream, "cas-dir="+f.cas, "bs=4096", "status=none"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, filepath.Join(f.dir, "stdout")), f.content) {
		t.Error("stdout differs from the image")
	}
}

func TestSkipAndCount(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		name string
		args []string
		want []byte
	}{
		{"blocks", []string{"bs=4096", "skip=1", "count=2"}, f.content[4096:12288]},
		{"bytes", []string{"bs=4096", "skip=10", "count=20", "iflag=skip_bytes,count_bytes"}, f.content[10:30]},
		{"across the hole", []string{"bs=1", "skip=10", "count=" + "5242900"}, f.content[10 : 10+5242900]},
		{"past the end", []string{"bs=4096", "skip=100000"}, nil},
		{"count past the end", []string{"count=1000"}, f.content},
	} {
		for _, in := range []string{f.raw, f.xstream, f.cstream} {
			out := filepath.Join(f.dir, "skipcount")
			args := append([]string{"if=" + in, "of=" + out, "cas-dir=" + f.cas, "status=none"}, c.args...)
			if _, err := f.dd(t, args...); err != nil {
				t.Fatalf("%s %s: %v", c.name, filepath.Base(in), err)
			}
			if got := readFile(t, out); !bytes.Equal(got, c.want) {
				t.Errorf("%s %s: got %d bytes, want %d", c.name, filepath.Base(in), len(got), len(c.want))
			}
		}
	}
}

func TestSeekAndNotrunc(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(f.dir, "existing")
	existing := bytes.Repeat([]byte{0xAA}, 3<<20)
	if err := os.WriteFile(out, existing, 0o644); err != nil {
		t.Fatal(err)
	}
	// Write 16 bytes of the image into the middle of the file.
	if _, err := f.dd(t, "if="+f.xstream, "of="+out, "cas-dir="+f.cas, "bs=16", "count=1", "seek=1000", "status=none"); err != nil {
		t.Fatal(err)
	}
	want := append([]byte{}, existing...)
	copy(want[16000:], f.content[:16])
	if !bytes.Equal(readFile(t, out), want) {
		t.Error("seek without truncating changed other bytes")
	}
}

func TestSparseIsOnlyTheDefaultWhereItIsSafe(t *testing.T) {
	f := newFixture(t)
	stale := bytes.Repeat([]byte{0xFF}, len(f.content)+100)
	write := func() string {
		out := filepath.Join(f.dir, "stale")
		if err := os.WriteFile(out, stale, 0o644); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// notrunc into an existing file: holes are written as zeros.
	out := write()
	if _, err := f.dd(t, "if="+f.xstream, "of="+out, "cas-dir="+f.cas, "conv=notrunc", "status=none"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, out); !bytes.Equal(got[:len(f.content)], f.content) || !bytes.Equal(got[len(f.content):], stale[len(f.content):]) {
		t.Error("notrunc did not write zeros over the stale data")
	}

	// conv=sparse leaves what is there: the caller promised zeros.
	out = write()
	if _, err := f.dd(t, "if="+f.xstream, "of="+out, "cas-dir="+f.cas, "conv=notrunc,sparse", "status=none"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, out)
	if !bytes.Equal(got[:17], f.content[:17]) || got[100] != 0xFF {
		t.Errorf("conv=sparse wrote the hole: %x", got[100])
	}

	// conv=nosparse writes zeros even into a new file.
	out = filepath.Join(f.dir, "dense")
	stderr, err := f.dd(t, "if="+f.xstream, "of="+out, "cas-dir="+f.cas, "conv=nosparse")
	if err != nil || !bytes.Equal(readFile(t, out), f.content) || strings.Contains(stderr, "skipped") {
		t.Errorf("nosparse: %v, %q", err, stderr)
	}
}

func TestTrailingHoleExtendsFile(t *testing.T) {
	dir := t.TempDir()
	var xs bytes.Buffer
	w := extentstream.NewWriter(&xs)
	w.AddData([]byte("data"))
	w.AddHole(1 << 20)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "x.xstream")
	os.WriteFile(in, xs.Bytes(), 0o644)
	out := filepath.Join(dir, "out")
	f := &fixture{dir: dir}
	if _, err := f.dd(t, "if="+in, "of="+out, "status=none"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() != 4+1<<20 {
		t.Fatalf("size %v, %v", st, err)
	}
	// notrunc into a shorter file extends it as well.
	os.WriteFile(out, []byte("xx"), 0o644)
	if _, err := f.dd(t, "if="+in, "of="+out, "conv=notrunc,sparse", "status=none"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(out); st.Size() != 4+1<<20 {
		t.Errorf("size %d", st.Size())
	}
}

func TestMissingBlobIsReported(t *testing.T) {
	f := newFixture(t)
	_, err := f.dd(t, "if="+f.xstream, "of="+filepath.Join(f.dir, "o"), "status=none")
	if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "IMG_REAPI_ENDPOINT") {
		t.Errorf("without blobs: %v", err)
	}
}

func TestProgressAndFsync(t *testing.T) {
	f := newFixture(t)
	stderr, err := f.dd(t, "if="+f.raw, "of="+filepath.Join(f.dir, "o"), "status=progress", "conv=fsync", "bs=64K")
	if err != nil || !strings.Contains(stderr, "100%") {
		t.Errorf("progress: %v, %q", err, stderr)
	}
}

func TestSeekNeedsAFile(t *testing.T) {
	f := newFixture(t)
	if _, err := f.dd(t, "if="+f.raw, "seek=1"); err == nil {
		t.Error("seek on stdout was accepted")
	}
}

// A raw image is written from its digest, as the blobs of a stream are.
func TestBlobDescriptor(t *testing.T) {
	f := newFixture(t)
	sum := sha256.Sum256(f.content)
	imagePath := casdirlayout.Path(filepath.Join(f.cas, "sha256"), hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(filepath.Dir(imagePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imagePath, f.content, 0o644); err != nil {
		t.Fatal(err)
	}
	desc := filepath.Join(f.dir, "image.json")
	write := func(content string) {
		if err := os.WriteFile(desc, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(fmt.Sprintf(`{"digest": "sha256:%x", "size": %d, "mediaType": "ignored"}`, sum, len(f.content)))

	out := filepath.Join(f.dir, "from-descriptor")
	stderr, err := f.dd(t, "blob-descriptor="+desc, "of="+out, "cas-dir="+f.cas)
	if err != nil || !bytes.Equal(readFile(t, out), f.content) || !strings.Contains(stderr, "skipped") {
		t.Fatalf("whole image: %v, %q", err, stderr)
	}
	if _, err := f.dd(t, "blob-descriptor="+desc, "of="+out, "cas-dir="+f.cas, "bs=4096", "skip=1281", "count=3", "status=none"); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, out), f.content[1281*4096:1284*4096]) {
		t.Error("range differs")
	}
	// Without the blob, it is reported.
	if _, err := f.dd(t, "blob-descriptor="+desc, "of="+out, "status=none"); err == nil || !strings.Contains(err.Error(), "IMG_REAPI_ENDPOINT") {
		t.Errorf("no blob: %v", err)
	}
	for _, bad := range []string{`{"digest": "sha256:zz", "size": 1}`, `{"digest": "md5:00", "size": 1}`, fmt.Sprintf(`{"digest": "sha256:%x"}`, sum), `nope`} {
		write(bad)
		if _, err := f.dd(t, "blob-descriptor="+desc, "of="+out, "status=none"); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
