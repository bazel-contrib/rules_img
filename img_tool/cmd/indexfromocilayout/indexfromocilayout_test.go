package indexfromocilayout

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/types"
	specv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/imagearchive/archivetest"
)

// platform is one requested manifest of the generated index.
type platform struct {
	os, arch, variant string
}

func (p platform) String() string {
	if p.variant != "" {
		return p.os + "/" + p.arch + "/" + p.variant
	}
	return p.os + "/" + p.arch
}

// writeLayout builds an OCI layout holding one single-layer image per platform,
// in the order given.
func writeLayout(t *testing.T, platforms []platform) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "oci_layout")
	lp, err := layout.Write(dir, empty.Index)
	if err != nil {
		t.Fatalf("layout.Write: %v", err)
	}
	for i, p := range platforms {
		layer := archivetest.MakeLayer(t, types.OCILayer, archivetest.Entry{
			Name: fmt.Sprintf("/file-%d", i),
			Body: fmt.Sprintf("content %d", i),
		})
		img := archivetest.MakeImage(t, p.os, p.arch, p.variant, nil, layer)
		if err := lp.AppendImage(img, layout.WithPlatform(v1.Platform{
			OS: p.os, Architecture: p.arch, Variant: p.variant,
		})); err != nil {
			t.Fatalf("AppendImage %s: %v", p, err)
		}
	}
	return dir
}

// runConvert converts srcLayout into an index for the given platforms and
// returns the platform of every manifest of the result, in index order.
func runConvert(t *testing.T, srcLayout string, platforms []platform) []string {
	t.Helper()
	out := t.TempDir()

	srcDir = srcLayout
	indexOutput = filepath.Join(out, "index.json")
	digestOutput = filepath.Join(out, "digest")
	indexDescriptorOutput = ""
	manifestPaths = newIndexedStringFlag()
	configPaths = newIndexedStringFlag()
	descriptorPaths = newIndexedStringFlag()
	osList = newIndexedStringFlag()
	architectureList = newIndexedStringFlag()
	variantList = newIndexedStringFlag()
	layerMediaTypes = newDoubleIndexedStringFlag()
	layerBlobs = newDoubleIndexedStringFlag()
	layerMetadataJSON = newDoubleIndexedStringFlag()

	set := func(f interface{ Set(string) error }, value string) {
		t.Helper()
		if err := f.Set(value); err != nil {
			t.Fatalf("setting %q: %v", value, err)
		}
	}
	for i, p := range platforms {
		set(manifestPaths, fmt.Sprintf("%d=%s", i, filepath.Join(out, fmt.Sprintf("m%d.json", i))))
		set(configPaths, fmt.Sprintf("%d=%s", i, filepath.Join(out, fmt.Sprintf("c%d.json", i))))
		set(descriptorPaths, fmt.Sprintf("%d=%s", i, filepath.Join(out, fmt.Sprintf("d%d.json", i))))
		set(osList, fmt.Sprintf("%d=%s", i, p.os))
		set(architectureList, fmt.Sprintf("%d=%s", i, p.arch))
		if p.variant != "" {
			set(variantList, fmt.Sprintf("%d=%s", i, p.variant))
		}
		set(layerMediaTypes, fmt.Sprintf("%d,0=%s", i, string(types.OCILayer)))
		set(layerBlobs, fmt.Sprintf("%d,0=%s", i, filepath.Join(out, fmt.Sprintf("l%d.tar", i))))
		set(layerMetadataJSON, fmt.Sprintf("%d,0=%s", i, filepath.Join(out, fmt.Sprintf("lm%d.json", i))))
	}

	if err := processOCILayoutIndex(); err != nil {
		t.Fatalf("processOCILayoutIndex: %v", err)
	}

	raw, err := os.ReadFile(indexOutput)
	if err != nil {
		t.Fatalf("reading index: %v", err)
	}
	var index specv1.Index
	if err := json.Unmarshal(raw, &index); err != nil {
		t.Fatalf("unmarshaling index: %v", err)
	}
	got := make([]string, 0, len(index.Manifests))
	for _, m := range index.Manifests {
		if m.Platform == nil {
			got = append(got, "<nil>")
			continue
		}
		got = append(got, platform{m.Platform.OS, m.Platform.Architecture, m.Platform.Variant}.String())
	}
	return got
}

// TestManifestOrderMatchesRequestedOrder pins the manifest order of a generated
// index to the order the manifests were requested in.
//
// The indices come from a map of flags, so collecting them without sorting
// yields Go's randomized map order, and since the descriptors are written back
// by position the manifests of the index get permuted. The command validates
// platforms in index order beforehand, so it still succeeds and writes an index
// whose manifests no longer sit where the caller said they would -- which only
// surfaces later, wherever that index is consumed. It also makes the output
// non-reproducible, so a build can cache a correct index and never notice.
//
// The permutation only happens on some runs, hence the repetitions.
func TestManifestOrderMatchesRequestedOrder(t *testing.T) {
	for _, platforms := range [][]platform{
		{{os: "linux", arch: "amd64"}, {os: "linux", arch: "arm64", variant: "v8"}},
		{{os: "linux", arch: "arm64", variant: "v8"}, {os: "linux", arch: "amd64"}},
		{
			{os: "linux", arch: "amd64"},
			{os: "linux", arch: "arm64", variant: "v8"},
			{os: "linux", arch: "ppc64le"},
			{os: "linux", arch: "s390x"},
		},
	} {
		t.Run(fmt.Sprint(len(platforms)), func(t *testing.T) {
			want := make([]string, 0, len(platforms))
			for _, p := range platforms {
				want = append(want, p.String())
			}
			src := writeLayout(t, platforms)
			for i := range 50 {
				got := runConvert(t, src, platforms)
				if len(got) != len(want) {
					t.Fatalf("run %d: index has %d manifests, want %d", i, len(got), len(want))
				}
				for j := range want {
					if got[j] != want[j] {
						t.Fatalf("run %d: manifest order %v, want %v", i, got, want)
					}
				}
			}
		})
	}
}
