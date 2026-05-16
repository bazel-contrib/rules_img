package deploy

import "github.com/bazel-contrib/rules_img/img_tool/pkg/api"

// waveGroup is the result of planPushWaves.
type waveGroup struct {
	// waves is the ordered list of push-operation batches.  Operations within a
	// single wave are pushed concurrently by remote.MultiWrite; waves are
	// executed sequentially so that layers uploaded in wave N are available for
	// cross-mounting by wave N+1 and beyond.
	waves [][]api.IndexedPushDeployOperation

	// crossMountHints maps each shared layer digest to the repository of the
	// first operation that will upload it.  These hints are applied to the VFS
	// before any wave runs so that later waves receive MountableLayer objects
	// and can skip re-uploading shared blobs.
	crossMountHints map[string]api.CrossMountSource
}

// planPushWaves partitions push operations into sequential waves so that each
// shared layer is uploaded exactly once and all later operations can cross-mount
// it from the first uploader's repository instead of re-uploading.
//
// Algorithm (single-pass greedy, O(ops × layers)):
//
//  1. For each operation in manifest order, find the latest wave W among all
//     operations that have already claimed a layer this operation also needs —
//     but only when both operations share the same registry host (cross-registry
//     blob mounts are not guaranteed by the OCI Distribution Specification).
//
//  2. Assign this operation to wave W+1.
//
//  3. Claim ownership of any layer not yet owned by an earlier operation.
//
// Result:
//   - Every shared layer is uploaded by exactly one operation (its first
//     claimant in manifest order).
//   - Each subsequent operation that needs the layer is placed in a later wave
//     and can cross-mount it server-side, transferring zero bytes.
//   - Operations that share layers with ops on different registries form no
//     dependency and execute concurrently in wave 1.
func planPushWaves(
	ops []api.IndexedPushDeployOperation,
	overrideRegistry, overrideRepository string,
) waveGroup {
	if len(ops) == 0 {
		return waveGroup{}
	}

	type layerOwner struct {
		wave     int    // 1-based wave of the first owner
		registry string // registry of the first owner
		repo     string // repository of the first owner
	}

	// Pre-compute total layer count for accurate map capacity hints.
	totalLayers := 0
	for _, op := range ops {
		for _, manifest := range op.Manifests {
			totalLayers += len(manifest.LayerBlobs)
		}
	}

	owned := make(map[string]layerOwner, totalLayers) // digest → first owner
	waveOf := make(map[int]int, len(ops))             // op.I → 1-based wave

	// Count how many distinct operations on each registry reference each digest.
	// This lets us suppress cross-mount hints for layers only shared across
	// different registries: OCI cross-registry mounts are not standardised, and
	// emitting a hint in that case would cause a wasted failed-mount round trip.
	digestRegistryCount := make(map[string]map[string]int, totalLayers)
	for _, op := range ops {
		opReg := op.Registry
		if overrideRegistry != "" {
			opReg = overrideRegistry
		}
		seen := make(map[string]struct{})
		for _, manifest := range op.Manifests {
			for _, layer := range manifest.LayerBlobs {
				if _, dup := seen[layer.Digest]; !dup {
					seen[layer.Digest] = struct{}{}
					if digestRegistryCount[layer.Digest] == nil {
						digestRegistryCount[layer.Digest] = make(map[string]int, 2)
					}
					digestRegistryCount[layer.Digest][opReg]++
				}
			}
		}
	}

	for _, op := range ops {
		opRegistry := op.Registry
		if overrideRegistry != "" {
			opRegistry = overrideRegistry
		}
		opRepo := op.Repository
		if overrideRepository != "" {
			opRepo = overrideRepository
		}

		// Find the latest wave this op must wait for.
		waitUntil := 0
		for _, manifest := range op.Manifests {
			for _, layer := range manifest.LayerBlobs {
				if o, found := owned[layer.Digest]; found &&
					o.registry == opRegistry && o.repo != opRepo {
					// Same registry, different repository: we can cross-mount
					// after o's wave completes.  Same-repo operations need no
					// wave dependency because remote.MultiWrite deduplicates
					// blobs within a repository via HEAD checks.
					if o.wave > waitUntil {
						waitUntil = o.wave
					}
				}
			}
		}

		myWave := waitUntil + 1
		waveOf[op.I] = myWave

		// Claim ownership of any layer not yet owned.
		for _, manifest := range op.Manifests {
			for _, layer := range manifest.LayerBlobs {
				if _, found := owned[layer.Digest]; !found {
					owned[layer.Digest] = layerOwner{
						wave:     myWave,
						registry: opRegistry,
						repo:     opRepo,
					}
				}
			}
		}
	}

	// Build the ordered wave slices.
	maxWave := 0
	for _, w := range waveOf {
		if w > maxWave {
			maxWave = w
		}
	}
	waves := make([][]api.IndexedPushDeployOperation, maxWave)
	for _, op := range ops {
		idx := waveOf[op.I] - 1 // convert to 0-based index
		waves[idx] = append(waves[idx], op)
	}

	// Emit a cross-mount hint for every layer where at least two operations on
	// the same registry share it.  The empty Registry field follows the
	// same-registry convention used by the existing cross-mount infrastructure.
	// Hints for layers only shared across different registries are suppressed:
	// the OCI Distribution Specification does not require cross-registry mount
	// support, so the hint would only produce a wasted failed-mount round trip.
	crossMountHints := make(map[string]api.CrossMountSource, len(owned))
	for digest, o := range owned {
		if digestRegistryCount[digest][o.registry] >= 2 {
			crossMountHints[digest] = api.CrossMountSource{
				Repository: o.repo,
			}
		}
	}

	return waveGroup{waves: waves, crossMountHints: crossMountHints}
}
