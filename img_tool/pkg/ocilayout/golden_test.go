package ocilayout

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// --- Golden fixtures ---------------------------------------------------------
//
// These tests pin the exact tar entry ORDER (and whole-tar bytes) of every
// tar-producing format so an accidental change in emission order or content is
// caught. Blob names are content-addressed, so the fixed fixture bytes below
// produce fully deterministic output.
//
// The canonical emission order these tests lock in is:
//   blobs/  ->  blobs/sha256/  ->  marker  ->  index.json|root.descriptor.json
//   ->  [manifest.json]  ->  all blob-ish entries sorted by path.
// The oci-layout single-manifest order in particular must stay byte-identical
// to the historical output pinned by tests/img_toolchain/testcases/ocilayout_tar.ini.
//
// To regenerate after an intentional change: run
//   go test ./pkg/ocilayout/ -run TestGolden -v
// read the logged "actual" order / sha256 for each case, and update the values.

func goldenConfig() []byte { return []byte(`{"architecture":"amd64","os":"linux"}`) }

// goldenImage builds a deterministic single-platform image (config + 2 layers)
// backed by in-memory blobs.
func goldenImage(arch string, layerA, layerB []byte) (ManifestInput, *MemBlobSource) {
	cfg := goldenConfig()
	src := NewMemBlobSource().
		Add(hashBytes(cfg).Hex, cfg).
		Add(hashBytes(layerA).Hex, layerA).
		Add(hashBytes(layerB).Hex, layerB)
	m := &v1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Config:        v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: hashBytes(cfg), Size: int64(len(cfg))},
		Layers: []v1.Descriptor{
			{MediaType: types.OCILayer, Digest: hashBytes(layerA), Size: int64(len(layerA))},
			{MediaType: types.OCILayer, Digest: hashBytes(layerB), Size: int64(len(layerB))},
		},
	}
	data, _ := json.Marshal(m)
	mi := ManifestInput{
		Manifest:     m,
		ManifestData: data,
		Config:       BlobFromSource(src, m.Config.Digest.Hex, m.Config.Size),
		Platform:     &v1.Platform{OS: "linux", Architecture: arch},
	}
	for _, l := range m.Layers {
		mi.Layers = append(mi.Layers, LayerInput{Descriptor: l, Blob: BlobFromSource(src, l.Digest.Hex, l.Size), Present: true})
	}
	return mi, src
}

func goldenIndexData(mi1, mi2 ManifestInput) []byte {
	idx := v1.IndexManifest{
		SchemaVersion: 2, MediaType: mediaTypeOCIImageIndex,
		Manifests: []v1.Descriptor{
			{MediaType: types.OCIManifestSchema1, Digest: hashBytes(mi1.ManifestData), Size: int64(len(mi1.ManifestData)), Platform: mi1.Platform},
			{MediaType: types.OCIManifestSchema1, Digest: hashBytes(mi2.ManifestData), Size: int64(len(mi2.ManifestData)), Platform: mi2.Platform},
		},
	}
	data, _ := json.Marshal(idx)
	return data
}

// orderedTarNames returns tar entry names in the exact order they appear.
func orderedTarNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		names = append(names, hdr.Name)
		if hdr.Typeflag != tar.TypeDir {
			io.Copy(io.Discard, tr)
		}
	}
	return names
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// assertGolden compares the produced tar's entry order and whole-tar sha256 to
// the wanted values, logging actuals to ease regeneration.
func assertGolden(t *testing.T, data []byte, wantOrder []string, wantSHA string) {
	t.Helper()
	got := orderedTarNames(t, data)
	gotSHA := sha256hex(data)
	if len(got) != len(wantOrder) {
		t.Errorf("entry count: got %d want %d", len(got), len(wantOrder))
	} else {
		for i := range got {
			if got[i] != wantOrder[i] {
				t.Errorf("entry order mismatch at [%d]: got %q want %q", i, got[i], wantOrder[i])
			}
		}
	}
	if gotSHA != wantSHA {
		t.Errorf("whole-tar sha256 mismatch:\n  got  %s\n  want %s", gotSHA, wantSHA)
	}
	if t.Failed() {
		t.Logf("actual order (%d entries):", len(got))
		for i, n := range got {
			t.Logf("  [%d] %s", i, n)
		}
		t.Logf("actual whole-tar sha256: %s", gotSHA)
	}
}

func TestGoldenDockerSaveSingle(t *testing.T) {
	mi, _ := goldenImage("amd64", []byte("layer-a-content"), []byte("layer-b-content"))
	var buf bytes.Buffer
	if err := New(DockerSave()).
		WithTags([]string{"repo:latest"}).
		WithOCITags([]string{"repo:latest"}).
		AddManifest(mi).
		WriteToWriter(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, buf.Bytes(), []string{
		"blobs/",
		"blobs/sha256/",
		"oci-layout",
		"index.json",
		"manifest.json",
		"blobs/sha256/4732f5e644711443a6b28688e9f8184c03a0dceb0655a3f6b3ccd2642ebd3fed",
		"blobs/sha256/9d99a75171aea000c711b34c0e5e3f28d3d537dd99d110eafbfbc2bd8e52c2bf",
		"blobs/sha256/b71a61ac32fda57eb383c5680ebfa3c6f7a8f6a3697089a3b6bf6c6c9d9de1e7",
		"blobs/sha256/e6f512c37cfc7dd2ad75879d145bb97d19e741f4167a73d07befaceae7b61bb1",
	}, "a06fa8454741962a363c86ec829dad9d90ecb2213f25c43211f9b3a613c55ae0")
}

func TestGoldenDockerSaveIndex(t *testing.T) {
	mi1, _ := goldenImage("amd64", []byte("amd64-a"), []byte("amd64-b"))
	mi2, _ := goldenImage("arm64", []byte("arm64-a"), []byte("arm64-b"))
	idxData := goldenIndexData(mi1, mi2)
	var buf bytes.Buffer
	if err := New(DockerSave().WithIndexStyle(IndexWrapping)).
		WithTags([]string{"repo:latest"}).
		WithOCITags([]string{"repo:latest"}).
		SetRootIndex(BlobFromBytes(idxData)).
		AddManifest(mi1).AddManifest(mi2).
		WriteToWriter(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, buf.Bytes(), []string{
		"blobs/",
		"blobs/sha256/",
		"oci-layout",
		"index.json",
		"manifest.json",
		"blobs/sha256/41491c6463f697cca0e5cd3690c8c4a0266e347505e7b4ae4e9c25d7e0587354",
		"blobs/sha256/61a4636eba6370d24d1a33f56c87024913c3aa06ef8230dd53868694b76baa9a",
		"blobs/sha256/6fb8060722e9a2ebe86baf0267646751b53ac8bd01e46a6d1414cc2104fb8587",
		"blobs/sha256/9d99a75171aea000c711b34c0e5e3f28d3d537dd99d110eafbfbc2bd8e52c2bf",
		"blobs/sha256/a834dbc3faa3e30c1a067af9d3f5ba74e06b5103795e88b71edc4b67a2599f73",
		"blobs/sha256/d8a71b73f625ed08354bca4bdd7fa8d0e957b0b9ab8f385e42ddee00d5a60358",
		"blobs/sha256/e79587156ac9da223079eaea3799dfe2d147b599528f772fc0c331ba170765a4",
		"blobs/sha256/f2eab175268c0a9100190111a71471dc4db7a71a768a984944776d528b787bf8",
	}, "df5df185012bfb680773d1fee717ac323623bfc13d4aac9da80e37479516591d")
}

func TestGoldenOCILayoutSingle(t *testing.T) {
	mi, _ := goldenImage("amd64", []byte("layer-a-content"), []byte("layer-b-content"))
	var buf bytes.Buffer
	if err := New(OCILayout()).AddManifest(mi).WriteToWriter(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, buf.Bytes(), []string{
		"blobs/",
		"blobs/sha256/",
		"oci-layout",
		"index.json",
		"blobs/sha256/4732f5e644711443a6b28688e9f8184c03a0dceb0655a3f6b3ccd2642ebd3fed",
		"blobs/sha256/9d99a75171aea000c711b34c0e5e3f28d3d537dd99d110eafbfbc2bd8e52c2bf",
		"blobs/sha256/b71a61ac32fda57eb383c5680ebfa3c6f7a8f6a3697089a3b6bf6c6c9d9de1e7",
		"blobs/sha256/e6f512c37cfc7dd2ad75879d145bb97d19e741f4167a73d07befaceae7b61bb1",
	}, "20993216aafbb2f710cf56af33afdc0bd75ea0e14687dfb4feca5e661dcf1c5c")
}

func TestGoldenSparseSingle(t *testing.T) {
	mi, _ := goldenImage("amd64", []byte("layer-a-content"), []byte("layer-b-content"))
	// Give the first layer a compact stream so its ordering is pinned too.
	cs := BlobFromBytes([]byte("compact-stream-bytes"))
	mi.Layers[0].CompactStream = &cs
	var buf bytes.Buffer
	if err := New(SparseOCILayout()).AddManifest(mi).WriteToWriter(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, buf.Bytes(), []string{
		"blobs/",
		"blobs/sha256/",
		"sparse-oci-layout",
		"root.descriptor.json",
		"blobs/sha256/4732f5e644711443a6b28688e9f8184c03a0dceb0655a3f6b3ccd2642ebd3fed",
		"blobs/sha256/9d99a75171aea000c711b34c0e5e3f28d3d537dd99d110eafbfbc2bd8e52c2bf",
		"blobs/sha256/b71a61ac32fda57eb383c5680ebfa3c6f7a8f6a3697089a3b6bf6c6c9d9de1e7.descriptor.json",
		"blobs/sha256/e6f512c37cfc7dd2ad75879d145bb97d19e741f4167a73d07befaceae7b61bb1.cstream",
		"blobs/sha256/e6f512c37cfc7dd2ad75879d145bb97d19e741f4167a73d07befaceae7b61bb1.descriptor.json",
	}, "90532c5ca2dcb615c00b99a518f554c04e8ba24fe0ad207246476698a359548a")
}

func TestGoldenSparseIndex(t *testing.T) {
	mi1, _ := goldenImage("amd64", []byte("amd64-a"), []byte("amd64-b"))
	mi2, _ := goldenImage("arm64", []byte("arm64-a"), []byte("arm64-b"))
	idxData := goldenIndexData(mi1, mi2)
	var buf bytes.Buffer
	if err := New(SparseOCILayout()).
		SetRootIndex(BlobFromBytes(idxData)).
		AddManifest(mi1).AddManifest(mi2).
		WriteToWriter(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, buf.Bytes(), []string{
		"blobs/",
		"blobs/sha256/",
		"sparse-oci-layout",
		"root.descriptor.json",
		"blobs/sha256/41491c6463f697cca0e5cd3690c8c4a0266e347505e7b4ae4e9c25d7e0587354",
		"blobs/sha256/61a4636eba6370d24d1a33f56c87024913c3aa06ef8230dd53868694b76baa9a.descriptor.json",
		"blobs/sha256/6fb8060722e9a2ebe86baf0267646751b53ac8bd01e46a6d1414cc2104fb8587",
		"blobs/sha256/9d99a75171aea000c711b34c0e5e3f28d3d537dd99d110eafbfbc2bd8e52c2bf",
		"blobs/sha256/a834dbc3faa3e30c1a067af9d3f5ba74e06b5103795e88b71edc4b67a2599f73.descriptor.json",
		"blobs/sha256/d8a71b73f625ed08354bca4bdd7fa8d0e957b0b9ab8f385e42ddee00d5a60358.descriptor.json",
		"blobs/sha256/e79587156ac9da223079eaea3799dfe2d147b599528f772fc0c331ba170765a4",
		"blobs/sha256/f2eab175268c0a9100190111a71471dc4db7a71a768a984944776d528b787bf8.descriptor.json",
	}, "686ebb9637d5016278d93a159c21fefc3729b832ca1823fba1cb5e8e7925b0cc")
}
