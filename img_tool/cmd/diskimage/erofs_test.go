package diskimage

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/erofscas"
)

// buildErofsRoot writes an EROFS layer with compressed files, as the image and
// as its compact stream, and puts the files it was made from into casDir.
func buildErofsRoot(t *testing.T, dir, casDir string, opts ...erofscas.Option) (image, stream string, size int64) {
	t.Helper()
	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", "none", &blob)
	if err != nil {
		t.Fatal(err)
	}
	var cs bytes.Buffer
	csWriter := compactstream.NewWriter(&cs, compactstream.HashAlgoSHA256, sha256.Size, compactstream.StreamCompressionZstd,
		compactstream.OriginalCompressionInfo{CompressionLevel: -1, CompressorJobs: 1}, 0)
	sink := erofscas.NewSHA256CASWithDigestFS(appender, digestfs.New(erofscas.SHA256Helper{}),
		append([]erofscas.Option{erofscas.WithCompactStreamWriter{Writer: csWriter}}, opts...)...)

	files := map[string][]byte{
		"usr/lib/text":    []byte(strings.Repeat("a line of text that compresses well\n", 60_000)),
		"usr/lib/random":  pattern(300_000, 7),
		"usr/lib/mixed":   append(pattern(40_000, 8), bytes.Repeat([]byte("xyz"), 90_000)...),
		"etc/os-release":  []byte("NAME=test\n"),
		"usr/share/small": bytes.Repeat([]byte("s"), 5000),
	}
	for name, content := range files {
		writeBlob(t, casDir, content)
		if err := sink.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644}, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	state, err := appender.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	if err := csWriter.SetCompressedStreamInfo(state.OuterHash, uint64(state.CompressedSize)); err != nil {
		t.Fatal(err)
	}
	if err := csWriter.Close(); err != nil {
		t.Fatal(err)
	}
	image, stream = filepath.Join(dir, "root.erofs"), filepath.Join(dir, "root.erofs.cstream")
	if err := os.WriteFile(image, blob.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stream, cs.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return image, stream, int64(blob.Len())
}

// TestCompressedErofsPartition checks that a disk whose root partition is a
// compressed EROFS layer is the same whether the partition comes as an image
// or as its compact stream. In the second case the extent stream of the disk
// references the compact stream of the layer, whose compressed files are
// transformed references: reconstructing the disk encodes them again.
func TestCompressedErofsPartition(t *testing.T) {
	for _, c := range []struct {
		name string
		opts []erofscas.Option
	}{
		{"lz4", []erofscas.Option{erofscas.LZ4FileCompression}},
		{"zstd", []erofscas.Option{erofscas.ZstdFileCompression}},
		{"deflate", []erofscas.Option{erofscas.DeflateFileCompression}},
		{"lz4-clusters", []erofscas.Option{erofscas.LZ4FileCompression, erofscas.ClusterBlocks(16)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixtures(t)
			image, stream, size := buildErofsRoot(t, f.dir, f.casDir, c.opts...)

			_, _, plainSize := buildErofsRoot(t, t.TempDir(), t.TempDir())
			if size >= plainSize/2 {
				t.Fatalf("the compressed layer is %d bytes, the uncompressed one %d", size, plainSize)
			}
			info, err := compactstream.Inspect(bytes.NewReader(mustRead(t, stream)))
			if err != nil || !info.Header.TransformedRefs {
				t.Fatalf("the layer's compact stream has no transformed references (err %v)", err)
			}

			parts := func(p *partitionConfig) []*partitionConfig {
				return []*partitionConfig{{label: "ESP", typ: "esp", image: f.espImage}, p}
			}
			raw := f.cfg(t, "erofs.raw.img", parts(&partitionConfig{label: "root", typ: "linux", image: image})...)
			raw.minSize = "100MiB"
			if err := run(context.Background(), raw); err != nil {
				t.Fatal(err)
			}

			// The partition as a compact stream, in a raw disk and in an extent stream.
			fromStream := f.cfg(t, "erofs.stream.img", parts(&partitionConfig{label: "root", typ: "linux", cstream: stream})...)
			fromStream.casDirs = []string{f.casDir}
			fromStream.minSize = "100MiB"
			if err := run(context.Background(), fromStream); err != nil {
				t.Fatal(err)
			}
			if got, want := fileDigest(t, fromStream.output), fileDigest(t, raw.output); got != want {
				t.Errorf("the disk built from the compact stream (%s) differs from the one built from the image (%s)", got, want)
			}

			casOut := filepath.Join(f.dir, "erofs.cas")
			xs := f.cfg(t, "erofs.xstream", parts(&partitionConfig{label: "root", typ: "linux", cstream: stream})...)
			xs.extentStream = true
			xs.minSize = "100MiB"
			xs.outputCASDir = casOut
			if err := run(context.Background(), xs); err != nil {
				t.Fatal(err)
			}
			gotDigest, gotSize := reconstructDigest(t, xs.output, casOut, f.casDir)
			st, _ := os.Stat(raw.output)
			if gotSize != st.Size() || gotDigest != fileDigest(t, raw.output) {
				t.Errorf("the extent stream reconstructs to %d bytes (%s), the raw disk has %d bytes (%s)", gotSize, gotDigest, st.Size(), fileDigest(t, raw.output))
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
