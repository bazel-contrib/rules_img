package deployvfs

import (
	"encoding/json"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

func TestWithCrossMountHints(t *testing.T) {
	const (
		digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	)

	t.Run("adds hints for digests without existing hints", func(t *testing.T) {
		// Empty manifest produces no base-image hints from ingest; extras fill in.
		vfs, err := Builder(api.DeployManifest{}).
			WithCrossMountHints(map[string]api.CrossMountSource{
				digestA: {Repository: "proj/frontend"},
				digestB: {Registry: "other.io", Repository: "proj/shared"},
			}).Build()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := vfs.crossMountHints[digestA], (api.CrossMountSource{Repository: "proj/frontend"}); got != want {
			t.Errorf("digestA: got %+v, want %+v", got, want)
		}
		if got, want := vfs.crossMountHints[digestB], (api.CrossMountSource{Registry: "other.io", Repository: "proj/shared"}); got != want {
			t.Errorf("digestB: got %+v, want %+v", got, want)
		}
	})

	t.Run("does not overwrite existing hints", func(t *testing.T) {
		// Build a manifest whose operation carries a CrossMountHint for digestA.
		// Using "cas_registry" strategy avoids needing real files: layerBlob()
		// returns a stub without touching the filesystem.
		opJSON, err := json.Marshal(api.BaseCommandOperation{
			Command:  "push",
			RootKind: "manifest",
			Root:     api.Descriptor{Digest: "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"},
			Manifests: []api.ManifestDeployInfo{{
				Descriptor: api.Descriptor{Digest: "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"},
				Config:     api.Descriptor{Digest: "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"},
				LayerBlobs: []api.Descriptor{{Digest: digestA}},
			}},
			CrossMountHint: &api.CrossMountSource{Registry: "base.io", Repository: "library/ubuntu"},
		})
		if err != nil {
			t.Fatal(err)
		}
		dm := api.DeployManifest{
			Operations: []json.RawMessage{opJSON},
			Settings:   api.DeploySettings{PushStrategy: "cas_registry"},
		}
		vfs, err := Builder(dm).
			WithCrossMountHints(map[string]api.CrossMountSource{
				digestA: {Repository: "proj/frontend"}, // must not overwrite base.io hint
				digestC: {Repository: "proj/backend"},  // must be added
			}).Build()
		if err != nil {
			t.Fatal(err)
		}
		wantA := api.CrossMountSource{Registry: "base.io", Repository: "library/ubuntu"}
		if got := vfs.crossMountHints[digestA]; got != wantA {
			t.Errorf("digestA: base-image hint was overwritten; got %+v, want %+v", got, wantA)
		}
		if got, want := vfs.crossMountHints[digestC], (api.CrossMountSource{Repository: "proj/backend"}); got != want {
			t.Errorf("digestC: got %+v, want %+v", got, want)
		}
	})

	t.Run("is a no-op when hints map is nil", func(t *testing.T) {
		vfs, err := Builder(api.DeployManifest{}).WithCrossMountHints(nil).Build()
		if err != nil {
			t.Fatal(err)
		}
		if len(vfs.crossMountHints) != 0 {
			t.Errorf("expected empty crossMountHints after nil, got %v", vfs.crossMountHints)
		}
	})
}
