package deploy

import (
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// op is a convenience constructor for test operations.
func op(i int, registry, repo string, layerDigests ...string) api.IndexedPushDeployOperation {
	var blobs []api.Descriptor
	for _, d := range layerDigests {
		blobs = append(blobs, api.Descriptor{Digest: d, Size: 1})
	}
	return api.IndexedPushDeployOperation{
		I: i,
		PushDeployOperation: api.PushDeployOperation{
			BaseCommandOperation: api.BaseCommandOperation{
				Manifests: []api.ManifestDeployInfo{{LayerBlobs: blobs}},
			},
			PushTarget: api.PushTarget{Registry: registry, Repository: repo},
		},
	}
}

// opIdx is like op but with multiple manifests (simulating an image index).
func opIdx(i int, registry, repo string, perManifestDigests ...[]string) api.IndexedPushDeployOperation {
	var manifests []api.ManifestDeployInfo
	for _, digests := range perManifestDigests {
		var blobs []api.Descriptor
		for _, d := range digests {
			blobs = append(blobs, api.Descriptor{Digest: d, Size: 1})
		}
		manifests = append(manifests, api.ManifestDeployInfo{LayerBlobs: blobs})
	}
	return api.IndexedPushDeployOperation{
		I: i,
		PushDeployOperation: api.PushDeployOperation{
			BaseCommandOperation: api.BaseCommandOperation{Manifests: manifests},
			PushTarget:           api.PushTarget{Registry: registry, Repository: repo},
		},
	}
}

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	digestD = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

// waveOps extracts, for each wave index, the set of operation indices (op.I).
func waveOps(wg waveGroup) [][]int {
	result := make([][]int, len(wg.waves))
	for i, wave := range wg.waves {
		for _, op := range wave {
			result[i] = append(result[i], op.I)
		}
	}
	return result
}

func TestPlanPushWaves(t *testing.T) {
	tests := []struct {
		name               string
		ops                []api.IndexedPushDeployOperation
		overrideRegistry   string
		overrideRepository string
		// wantWaves is the expected wave assignment: wantWaves[i] is the set of
		// op.I values in wave i.  Order within a wave does not matter.
		wantWaves [][]int
		// wantHints is the expected cross-mount hints map.
		wantHints map[string]api.CrossMountSource
	}{
		{
			name:      "no operations",
			ops:       nil,
			wantWaves: nil,
			wantHints: map[string]api.CrossMountSource{},
		},
		{
			name:      "single operation, no sharing",
			ops:       []api.IndexedPushDeployOperation{op(0, "r.io", "proj/a", digestA)},
			wantWaves: [][]int{{0}},
			wantHints: map[string]api.CrossMountSource{},
		},
		{
			// 6 images all share one base layer → wave 1 = first image,
			// wave 2 = remaining 5 (cross-mount base).
			name: "6 images share one layer",
			ops: []api.IndexedPushDeployOperation{
				op(0, "r.io", "proj/svc-a", digestA, digestB),
				op(1, "r.io", "proj/svc-b", digestA, digestC),
				op(2, "r.io", "proj/svc-c", digestA, digestD),
				op(3, "r.io", "proj/svc-d", digestA),
				op(4, "r.io", "proj/svc-e", digestA),
				op(5, "r.io", "proj/svc-f", digestA),
			},
			wantWaves: [][]int{
				{0},             // wave 1: uploads digestA (and digestB)
				{1, 2, 3, 4, 5}, // wave 2: cross-mount digestA from proj/svc-a
			},
			wantHints: map[string]api.CrossMountSource{
				digestA: {Repository: "proj/svc-a"},
			},
		},
		{
			// Two images with entirely disjoint layers → both in wave 1, no hints.
			name: "two images no shared layers",
			ops: []api.IndexedPushDeployOperation{
				op(0, "r.io", "proj/a", digestA),
				op(1, "r.io", "proj/b", digestB),
			},
			wantWaves: [][]int{{0, 1}},
			wantHints: map[string]api.CrossMountSource{},
		},
		{
			// A={X,Y}, B={X,Z}, C={Y,Z}
			// A owns X,Y (wave 1); B needs X from A (wave 2, claims Z);
			// C needs Y from A (wave 2) and Z from B (wave 3) → wave 3.
			name: "transitive dependency chain",
			ops: []api.IndexedPushDeployOperation{
				op(0, "r.io", "proj/a", digestA, digestB),
				op(1, "r.io", "proj/b", digestA, digestC),
				op(2, "r.io", "proj/c", digestB, digestC),
			},
			wantWaves: [][]int{
				{0},
				{1},
				{2},
			},
			wantHints: map[string]api.CrossMountSource{
				digestA: {Repository: "proj/a"},
				digestB: {Repository: "proj/a"},
				digestC: {Repository: "proj/b"},
			},
		},
		{
			// Different registries: no wave dependency, all in wave 1.
			// No hint is emitted: OCI cross-registry mounts are not standardised,
			// so a hint would only produce a wasted failed-mount round trip.
			name:      "different registries no dependency",
			ops: []api.IndexedPushDeployOperation{
				op(0, "reg1.io", "proj/a", digestA),
				op(1, "reg2.io", "proj/b", digestA),
			},
			wantWaves: [][]int{{0, 1}},
			wantHints: map[string]api.CrossMountSource{},
		},
		{
			// Same registry AND same repo → no dependency (mount from self),
			// all in wave 1, but a hint is emitted (harmless self-mount attempt).
			name: "same registry same repo",
			ops: []api.IndexedPushDeployOperation{
				op(0, "r.io", "proj/img", digestA),
				op(1, "r.io", "proj/img", digestA),
			},
			wantWaves: [][]int{{0, 1}},
			wantHints: map[string]api.CrossMountSource{
				digestA: {Repository: "proj/img"},
			},
		},
		{
			// Override registry collapses two "different-registry" ops onto the
			// same registry, creating a wave dependency.
			name: "override registry creates dependency",
			ops: []api.IndexedPushDeployOperation{
				op(0, "old1.io", "proj/a", digestA),
				op(1, "old2.io", "proj/b", digestA),
			},
			overrideRegistry: "new.io",
			wantWaves: [][]int{
				{0},
				{1},
			},
			wantHints: map[string]api.CrossMountSource{
				digestA: {Repository: "proj/a"},
			},
		},
		{
			// Image index: two manifests per op share a layer with another op.
			name: "index with shared layer across ops",
			ops: []api.IndexedPushDeployOperation{
				opIdx(0, "r.io", "proj/multi", []string{digestA}, []string{digestB}),
				op(1, "r.io", "proj/single", digestA),
			},
			wantWaves: [][]int{
				{0},
				{1},
			},
			wantHints: map[string]api.CrossMountSource{
				digestA: {Repository: "proj/multi"},
			},
		},
		{
			// overrideRepository collapses two distinct repos into one → same-repo
			// rule applies, no wave dependency, both in wave 1.
			name: "override repository creates same-repo, no dependency",
			ops: []api.IndexedPushDeployOperation{
				op(0, "r.io", "proj/a", digestA),
				op(1, "r.io", "proj/b", digestA),
			},
			overrideRepository: "proj/unified",
			wantWaves:          [][]int{{0, 1}},
			wantHints: map[string]api.CrossMountSource{
				digestA: {Repository: "proj/unified"},
			},
		},
		{
			// A single multi-manifest op (image index) where the same digest
			// appears in more than one of its own manifests.  The per-op dedup
			// must ensure digestRefCount stays at 1, so no spurious hint is
			// emitted for a layer that is not actually shared across operations.
			name: "same digest in multiple manifests of one op, no hint",
			ops: []api.IndexedPushDeployOperation{
				opIdx(0, "r.io", "proj/multi", []string{digestA, digestB}, []string{digestA, digestC}),
			},
			wantWaves: [][]int{{0}},
			wantHints: map[string]api.CrossMountSource{},
		},
		{
			// An operation with no layers at all goes to wave 1 without error.
			name: "op with no layers",
			ops: []api.IndexedPushDeployOperation{
				op(0, "r.io", "proj/scratch"),
			},
			wantWaves: [][]int{{0}},
			wantHints: map[string]api.CrossMountSource{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := planPushWaves(tc.ops, tc.overrideRegistry, tc.overrideRepository)

			// Check wave structure.
			gotWaves := waveOps(got)
			if len(gotWaves) != len(tc.wantWaves) {
				t.Errorf("got %d waves, want %d\ngot:  %v\nwant: %v", len(gotWaves), len(tc.wantWaves), gotWaves, tc.wantWaves)
			} else {
				for i := range tc.wantWaves {
					gotSet := intSet(gotWaves[i])
					wantSet := intSet(tc.wantWaves[i])
					for id := range wantSet {
						if !gotSet[id] {
							t.Errorf("wave %d: want op %d but not present; got %v", i, id, gotWaves[i])
						}
					}
					for id := range gotSet {
						if !wantSet[id] {
							t.Errorf("wave %d: unexpected op %d; got %v, want %v", i, id, gotWaves[i], tc.wantWaves[i])
						}
					}
				}
			}

			// Check hints.
			wantHints := tc.wantHints
			if wantHints == nil {
				wantHints = map[string]api.CrossMountSource{}
			}
			if len(got.crossMountHints) != len(wantHints) {
				t.Errorf("got %d hints, want %d\ngot:  %v\nwant: %v", len(got.crossMountHints), len(wantHints), got.crossMountHints, wantHints)
			}
			for digest, wantHint := range wantHints {
				gotHint, ok := got.crossMountHints[digest]
				if !ok {
					t.Errorf("missing hint for %s", digest)
					continue
				}
				if gotHint != wantHint {
					t.Errorf("digest %s: got hint %+v, want %+v", digest, gotHint, wantHint)
				}
			}
			for digest := range got.crossMountHints {
				if _, ok := wantHints[digest]; !ok {
					t.Errorf("unexpected hint for %s: %+v", digest, got.crossMountHints[digest])
				}
			}
		})
	}
}

func intSet(ids []int) map[int]bool {
	s := make(map[int]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}
