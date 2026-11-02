package deployvfs

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/digestfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/erofscas"
)

// TestCompressedErofsLayerFromCompactStream pushes the way deploy does: the layer
// blob is not shipped, only its compact stream and the files it was made from.
// The stream's compressed files are transformed references, so reconstructing the
// blob encodes them again; the result has to be the exact blob, because its
// digest is what the manifest names.
func TestCompressedErofsLayerFromCompactStream(t *testing.T) {
	files := map[string][]byte{
		"usr/lib/text":   []byte(strings.Repeat("deploy reads this back out of a compact stream\n", 40_000)),
		"usr/lib/random": bytes.Repeat([]byte{0x13, 0x37, 0xc0, 0xde, 0x42, 0x99, 0x01, 0x7f, 0xaa, 0x55}, 2000),
		"etc/os-release": []byte("NAME=test\n"),
	}
	for name, opts := range map[string][]erofscas.Option{
		"lz4":          {erofscas.LZ4FileCompression},
		"zstd":         {erofscas.ZstdFileCompression},
		"deflate":      {erofscas.DeflateFileCompression},
		"lz4-clusters": {erofscas.LZ4FileCompression, erofscas.ClusterBlocks(16)},
	} {
		for _, outer := range []string{"none", "zstd"} {
			t.Run(name+"/"+outer, func(t *testing.T) {
				layout := t.TempDir()

				var blob bytes.Buffer
				appender, err := compress.AppenderFactory("sha256", outer, &blob)
				if err != nil {
					t.Fatal(err)
				}
				var cs bytes.Buffer
				origComp := compactstream.OriginalCompressionNone
				if outer == "zstd" {
					origComp = compactstream.OriginalCompressionZstd
				}
				csWriter := compactstream.NewWriter(&cs, compactstream.HashAlgoSHA256, sha256.Size, compactstream.StreamCompressionZstd,
					compactstream.OriginalCompressionInfo{Compression: origComp, CompressionLevel: -1, CompressorJobs: 1}, 0)
				sink := erofscas.NewSHA256CASWithDigestFS(appender, digestfs.New(erofscas.SHA256Helper{}),
					append([]erofscas.Option{erofscas.WithCompactStreamWriter{Writer: csWriter}}, opts...)...)
				for fileName, content := range files {
					shipCASBlob(t, layout, content)
					if err := sink.WriteRegular(&tar.Header{Typeflag: tar.TypeReg, Name: fileName, Size: int64(len(content)), Mode: 0o644}, bytes.NewReader(content)); err != nil {
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

				info, err := compactstream.Inspect(bytes.NewReader(cs.Bytes()))
				if err != nil || !info.Header.TransformedRefs {
					t.Fatalf("the compact stream has no transformed references (err %v)", err)
				}

				digest := sha256.Sum256(blob.Bytes())
				hexDigest := hex.EncodeToString(digest[:])
				writeBlobFile(t, filepath.Join(layout, "blobs", "sha256", hexDigest+".cstream"), cs.Bytes())

				b := NewBuilder(api.DeployManifest{}).WithOCILayout(layout)
				entry, err := b.layerFromOCILayoutCompactStream(api.Descriptor{Digest: "sha256:" + hexDigest, Size: int64(blob.Len())})
				if err != nil {
					t.Fatalf("layerFromOCILayoutCompactStream: %v", err)
				}
				if got := readAllClose(t, mustOpen(t, entry)); !bytes.Equal(got, blob.Bytes()) {
					t.Fatalf("the reconstructed layer (%d bytes) is not the layer that was built (%d bytes)", len(got), blob.Len())
				}
			})
		}
	}
}
