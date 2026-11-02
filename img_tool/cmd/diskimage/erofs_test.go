package diskimage

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/erofscas"
)

// buildErofsRoot writes an EROFS layer with the given options.
func buildErofsRoot(t *testing.T, dir string, opts ...erofscas.Option) (image string, size int64) {
	t.Helper()
	var blob bytes.Buffer
	appender, err := compress.AppenderFactory("sha256", "none", &blob)
	if err != nil {
		t.Fatal(err)
	}
	sink := erofscas.NewSHA256CASWithDigestFS(appender, digestfs.New(erofscas.SHA256Helper{}), opts...)

	files := map[string][]byte{
		"usr/lib/text":    []byte(strings.Repeat("a line of text that compresses well\n", 60_000)),
		"usr/lib/random":  pattern(300_000, 7),
		"usr/lib/mixed":   append(pattern(40_000, 8), bytes.Repeat([]byte("xyz"), 90_000)...),
		"etc/os-release":  []byte("NAME=test\n"),
		"usr/share/small": bytes.Repeat([]byte("s"), 5000),
	}
	for name, content := range files {
		if err := sink.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(content)), Mode: 0o644}, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := appender.Finalize(); err != nil {
		t.Fatal(err)
	}
	image = filepath.Join(dir, "root.erofs")
	if err := os.WriteFile(image, blob.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return image, int64(blob.Len())
}

// TestCompressedErofsPartition checks that a compressed EROFS layer is written
// into a disk as it is, and that the disk is smaller for it.
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
			image, size := buildErofsRoot(t, f.dir, c.opts...)
			_, plainSize := buildErofsRoot(t, t.TempDir())
			if size >= plainSize/2 {
				t.Fatalf("the compressed layer is %d bytes, the uncompressed one %d", size, plainSize)
			}

			cfg := f.cfg(t, "erofs.raw.img",
				&partitionConfig{label: "ESP", typ: "esp", image: f.espImage},
				&partitionConfig{label: "root", typ: "linux", image: image})
			cfg.minSize = "100MiB"
			if err := run(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(mustRead(t, cfg.output), mustRead(t, image)) {
				t.Error("the disk does not contain the compressed layer")
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
