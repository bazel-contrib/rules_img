package deploymetadata

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildBuildInfo(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	manifest := `{"layers":[{"digest":"sha256:aa"},{"digest":"sha256:bb"}]}`
	sum := sha256.Sum256([]byte(manifest))
	entry := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	layer := entry(map[string]any{"kind": "layer", "label": "//a:layer", "config": "cfg", "file": write("layer.json", `{"digest":"sha256:bb"}`)})
	entries := []string{
		entry(map[string]any{"kind": "image", "label": "//a:img", "config": "cfg", "file": write("m.json", manifest), "base": "@base//:base", "platform": map[string]any{"os": "linux", "architecture": "arm64", "variant": "v8"}}),
		layer,
		layer, // duplicates from several paths in the graph collapse
		entry(map[string]any{"kind": "pull", "label": "@base//:base", "config": "cfg", "pull": map[string]any{"registries": []string{"ghcr.io"}, "repository": "o/base", "tag": "1", "digest": "sha256:cc"}}),
	}
	got, err := buildBuildInfo(BuildInfoTarget{Label: "//a:push", Configuration: "cfg"}, entries)
	if err != nil {
		t.Fatal(err)
	}
	want := &BuildInfo{
		SchemaVersion: 1,
		Deploy:        BuildInfoTarget{Label: "//a:push", Configuration: "cfg"},
		Images: []BuildInfoImage{{
			BuildInfoTarget: BuildInfoTarget{Label: "//a:img", Configuration: "cfg"}, Kind: "image",
			Digest: "sha256:" + hex.EncodeToString(sum[:]), Base: "@base//:base", Platform: &BuildInfoPlatform{OS: "linux", Architecture: "arm64", Variant: "v8"}, Layers: []string{"sha256:aa", "sha256:bb"},
		}},
		Layers:     []BuildInfoLayer{{BuildInfoTarget: BuildInfoTarget{Label: "//a:layer", Configuration: "cfg"}, Digest: "sha256:bb"}},
		BaseImages: []BuildInfoBaseImage{{Label: "@base//:base", Registries: []string{"ghcr.io"}, Repository: "o/base", Tag: "1", Digest: "sha256:cc"}},
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", " ")
		t.Errorf("got %s", gotJSON)
	}
}
