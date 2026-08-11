package deploy

import (
	"strings"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

const (
	sharedLayerDigest  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	serviceALayerDiges = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	serviceBLayerDiges = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
	manifestADigest    = "sha256:aaaa111111111111111111111111111111111111111111111111111111111111"
	manifestBDigest    = "sha256:bbbb222222222222222222222222222222222222222222222222222222222222"
)

// layer builds a layer blob descriptor, optionally recording the repositories it
// is already available from.
func layer(digest string, sources ...api.LayerSource) api.LayerBlob {
	return api.LayerBlob{
		Descriptor: api.Descriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    digest,
			Size:      100,
		},
		Sources: sources,
	}
}

// pushOp builds a single-manifest push operation for the given destination.
func pushOp(registry, repository, manifestDigest string, layers ...api.LayerBlob) api.IndexedPushDeployOperation {
	return api.IndexedPushDeployOperation{
		PushDeployOperation: api.PushDeployOperation{
			BaseCommandOperation: api.BaseCommandOperation{
				Command:          "push",
				RootKind:         "manifest",
				DeduplicatedPush: true,
				Root:             api.Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: manifestDigest},
				Manifests: []api.ManifestDeployInfo{{
					Descriptor: api.Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: manifestDigest},
					Config:     api.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: "sha256:c0c0000000000000000000000000000000000000000000000000000000000000"},
					LayerBlobs: layers,
				}},
			},
			PushTarget: api.PushTarget{Registry: registry, Repository: repository},
		},
	}
}

// planFor runs the planner over the given operations with no manifest already
// present and no build-time staging repository -- the default configuration.
func planFor(t *testing.T, ops ...api.IndexedPushDeployOperation) *dedupPlan {
	t.Helper()
	plan, err := planDedupPush(dedupWorkingSet(ops, nil, dedupSelector{}, "", ""), nil, "")
	if err != nil {
		t.Fatalf("planDedupPush: %v", err)
	}
	return plan
}

// uploadedDigests returns the digests the plan uploads to the given full
// repository ("<registry>/<repository>"), as strings.
func uploadedDigests(plan *dedupPlan, repository string) []string {
	var digests []string
	for _, hash := range plan.uploads[repository] {
		digests = append(digests, hash.String())
	}
	return digests
}

// The registry the single-registry tests below push to.
const testRegistry = "reg.example.com"

// sourceIn returns the cross-mount source the plan recorded for a blob in a
// registry, or the zero value when it recorded none.
func sourceIn(plan *dedupPlan, registry, digest string) api.CrossMountSource {
	return plan.sources[registry][digest]
}

// mountOnlyIn reports whether the plan uploaded a blob to its mount source in a
// registry, which is what makes the layer mount-only there.
func mountOnlyIn(plan *dedupPlan, registry, digest string) bool {
	_, found := plan.mountOnly[registry][digest]
	return found
}

// countSources returns how many (registry, blob) pairs the plan mounts, and
// countMountOnly how many of those the deploy uploads itself.
func countSources(plan *dedupPlan) int {
	count := 0
	for _, sources := range plan.sources {
		count += len(sources)
	}
	return count
}

func countMountOnly(plan *dedupPlan) int {
	count := 0
	for _, digests := range plan.mountOnly {
		count += len(digests)
	}
	return count
}

// TestPlanDedupPushPicksTheFirstRepositoryAsHome is the case the strategy exists
// for: several services in different repositories of one registry sharing a base
// layer. The shared layer is uploaded once, to the lexicographically first of the
// repositories that need it, and cross-mounted into the rest.
func TestPlanDedupPushPicksTheFirstRepositoryAsHome(t *testing.T) {
	plan := planFor(t,
		pushOp("reg.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest), layer(serviceBLayerDiges)),
		pushOp("reg.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest), layer(serviceALayerDiges)),
	)

	if plan.total != 2 || plan.present != 0 {
		t.Errorf("plan counted %d/%d manifests present, want 0/2", plan.present, plan.total)
	}
	// Only the shared layer is worth deduplicating; each service's own layer has a
	// single destination and is left to the ordinary push.
	if got := strings.Join(uploadedDigests(plan, "reg.example.com/team/service-a"), ","); got != sharedLayerDigest {
		t.Errorf("uploads to team/service-a = %v, want just the shared layer", got)
	}
	if len(plan.uploads) != 1 {
		t.Errorf("plan uploads to %d repositories, want only the home repository: %v", len(plan.uploads), plan.uploads)
	}
	if src := sourceIn(plan, testRegistry, sharedLayerDigest); src.Repository != "team/service-a" || src.Registry != testRegistry {
		t.Errorf("source for the shared layer = %+v, want the home repository team/service-a in %s", src, testRegistry)
	}
	if !mountOnlyIn(plan, testRegistry, sharedLayerDigest) {
		t.Error("the shared layer is not mount-only, but this deploy uploaded it to its home repository")
	}
	if plan.mounted != 1 {
		t.Errorf("plan cross-mounts %d times, want 1 (service-b mounts from service-a)", plan.mounted)
	}
	// The two per-service layers.
	if plan.skipped != 2 {
		t.Errorf("plan left %d layers to the ordinary push, want the 2 single-destination layers", plan.skipped)
	}
	for _, digest := range []string{serviceALayerDiges, serviceBLayerDiges} {
		if src := sourceIn(plan, testRegistry, digest); src.Repository != "" {
			t.Errorf("%s got the cross-mount source %+v, but only one repository needs it", digest, src)
		}
		if mountOnlyIn(plan, testRegistry, digest) {
			t.Errorf("%s is mount-only, but only one repository needs it", digest)
		}
	}
}

// TestPlanDedupPushLeavesSingleImageDeploysAlone verifies the strategy is a no-op
// when there is nothing to deduplicate: with one destination repository, every
// layer keeps the ordinary push's behavior instead of paying an upload phase and a
// second HEAD.
func TestPlanDedupPushLeavesSingleImageDeploysAlone(t *testing.T) {
	plan := planFor(t, pushOp("reg.example.com", "team/service-a", manifestADigest,
		layer(sharedLayerDigest), layer(serviceALayerDiges)))

	if len(plan.uploads) != 0 {
		t.Errorf("plan uploads %v, want nothing for a single-repository deploy", plan.uploads)
	}
	if countSources(plan) != 0 || countMountOnly(plan) != 0 {
		t.Errorf("plan recorded sources %v / mount-only %v, want neither", plan.sources, plan.mountOnly)
	}
	if plan.skipped != 2 {
		t.Errorf("plan left %d layers to the ordinary push, want both", plan.skipped)
	}
}

// TestPlanDedupPushUsesTheBuildTimeStagingRepository verifies that a blob
// repository recorded in the deploy manifest -- which only happens when push at
// build time actually staged every blob there -- stays the home repository.
func TestPlanDedupPushUsesTheBuildTimeStagingRepository(t *testing.T) {
	ops := []api.IndexedPushDeployOperation{
		pushOp("reg.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
		pushOp("reg.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest)),
	}
	plan, err := planDedupPush(dedupWorkingSet(ops, nil, dedupSelector{}, "", ""), nil, "shared/blobs")
	if err != nil {
		t.Fatalf("planDedupPush: %v", err)
	}

	if src := sourceIn(plan, testRegistry, sharedLayerDigest); src.Repository != "shared/blobs" {
		t.Errorf("source for the shared layer = %+v, want the staging repository", src)
	}
	if got := strings.Join(uploadedDigests(plan, "reg.example.com/shared/blobs"), ","); got != sharedLayerDigest {
		t.Errorf("uploads to the staging repository = %v, want the shared layer", got)
	}
	// Both destinations mount from the staging repository; neither is the home.
	if plan.mounted != 2 {
		t.Errorf("plan cross-mounts %d times, want 2", plan.mounted)
	}
}

// TestPlanDedupPushMountsUpstreamLayers verifies that a layer recorded as already
// available in the destination registry (a shallow base image) is mounted from
// there instead of being downloaded and re-uploaded, and that it keeps its
// byte-upload fallback: we know the blob was there when the base image was pulled,
// not that it still is.
func TestPlanDedupPushMountsUpstreamLayers(t *testing.T) {
	base := layer(sharedLayerDigest, api.LayerSource{Registry: "reg.example.com", Repository: "library/base"})
	plan := planFor(t,
		pushOp("reg.example.com", "team/service-a", manifestADigest, base, layer(serviceALayerDiges)),
		pushOp("reg.example.com", "team/service-b", manifestBDigest, base, layer(serviceBLayerDiges)),
	)

	if src := sourceIn(plan, testRegistry, sharedLayerDigest); src.Repository != "library/base" {
		t.Errorf("source for the base layer = %+v, want library/base", src)
	}
	if mountOnlyIn(plan, testRegistry, sharedLayerDigest) {
		t.Error("the base layer is mount-only, but this deploy did not upload it")
	}
	if plan.upstream != 1 {
		t.Errorf("plan mounts %d layers from upstream repositories, want 1", plan.upstream)
	}
	if len(plan.uploads) != 0 {
		t.Errorf("plan uploads %v, want nothing (the base layer is mounted, the rest is single-destination)", plan.uploads)
	}
}

// TestPlanDedupPushUploadsLayersFromAnotherRegistry verifies that a layer whose
// only recorded source is in a different registry gets a home repository: blobs
// cannot be mounted across registries.
func TestPlanDedupPushUploadsLayersFromAnotherRegistry(t *testing.T) {
	base := layer(sharedLayerDigest, api.LayerSource{Registry: "other.example.com", Repository: "library/base"})
	plan := planFor(t,
		pushOp("reg.example.com", "team/service-a", manifestADigest, base),
		pushOp("reg.example.com", "team/service-b", manifestBDigest, base),
	)

	if src := sourceIn(plan, testRegistry, sharedLayerDigest); src.Repository != "team/service-a" {
		t.Errorf("source for the base layer = %+v, want the home repository team/service-a", src)
	}
	if !mountOnlyIn(plan, testRegistry, sharedLayerDigest) {
		t.Error("an uploaded layer must be mount-only")
	}
}

// TestPlanDedupPushMountsFromAPresentSibling is the incremental deploy: one
// service changed, the others did not. The manifests the registry already holds
// prove their blobs are there, so the changed service's shared layer is mounted out
// of one of them and nothing is uploaded at all.
func TestPlanDedupPushMountsFromAPresentSibling(t *testing.T) {
	ops := []api.IndexedPushDeployOperation{
		pushOp("reg.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest), layer(serviceALayerDiges)),
		pushOp("reg.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest), layer(serviceBLayerDiges)),
	}
	present := map[destination]bool{
		{registry: "reg.example.com", repository: "team/service-b", digest: manifestBDigest}: true,
	}
	plan, err := planDedupPush(dedupWorkingSet(ops, nil, dedupSelector{}, "", ""), present, "")
	if err != nil {
		t.Fatalf("planDedupPush: %v", err)
	}

	if plan.present != 1 || plan.total != 2 {
		t.Errorf("plan counted %d/%d manifests present, want 1/2", plan.present, plan.total)
	}
	if len(plan.uploads) != 0 {
		t.Errorf("plan uploads %v, want nothing: service-b already has the shared layer", plan.uploads)
	}
	if src := sourceIn(plan, testRegistry, sharedLayerDigest); src.Repository != "team/service-b" {
		t.Errorf("source for the shared layer = %+v, want the present sibling team/service-b", src)
	}
	// Mounting rests on an inference, not on something this deploy did, so the
	// byte-upload fallback stays in place.
	if mountOnlyIn(plan, testRegistry, sharedLayerDigest) {
		t.Error("the shared layer is mount-only, but this deploy did not upload it")
	}
	if plan.confirmed != 1 {
		t.Errorf("plan mounted %d layers out of a repository the registry serves, want 1", plan.confirmed)
	}
	if src := sourceIn(plan, testRegistry, serviceBLayerDiges); src.Repository != "" {
		t.Errorf("service B's own layer got the cross-mount source %+v even though its manifest is present", src)
	}
}

// TestPlanDedupPushDeduplicatesEachRegistrySeparately verifies the multi-registry
// case: a mount source is a repository *in a registry*, so each registry gets its own
// plan for the same blob. Pushing the same two repositories to two registries uploads
// the shared layer once per registry -- to that registry's first repository
// alphabetically -- and cross-mounts it into the other one there.
func TestPlanDedupPushDeduplicatesEachRegistrySeparately(t *testing.T) {
	plan := planFor(t,
		pushOp("a.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest)),
		pushOp("a.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
		pushOp("b.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest)),
		pushOp("b.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
	)

	for _, registry := range []string{"a.example.com", "b.example.com"} {
		if src := sourceIn(plan, registry, sharedLayerDigest); src.Repository != "team/service-a" || src.Registry != registry {
			t.Errorf("source for the shared layer in %s = %+v, want team/service-a in %s", registry, src, registry)
		}
		if !mountOnlyIn(plan, registry, sharedLayerDigest) {
			t.Errorf("the shared layer is not mount-only in %s, but this deploy uploaded it there", registry)
		}
		if got := strings.Join(uploadedDigests(plan, registry+"/team/service-a"), ","); got != sharedLayerDigest {
			t.Errorf("uploads to %s/team/service-a = %v, want the shared layer", registry, got)
		}
	}
	if len(plan.uploads) != 2 {
		t.Errorf("plan uploads to %d repositories, want the home repository in each registry: %v", len(plan.uploads), plan.uploads)
	}
	if plan.mounted != 2 || plan.skipped != 0 {
		t.Errorf("plan cross-mounts %d times and skipped %d layers, want 2 and 0 (one mount per registry)", plan.mounted, plan.skipped)
	}
}

// TestPlanDedupPushMixesSourcesPerRegistry is the point of planning per registry: the
// same layer can come from a different place in each. Registry a already serves a
// manifest referencing it (mount, no upload); registry b does not, so it uploads the
// layer to its own first repository and mounts it from there.
func TestPlanDedupPushMixesSourcesPerRegistry(t *testing.T) {
	ops := []api.IndexedPushDeployOperation{
		pushOp("a.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
		pushOp("a.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest)),
		pushOp("b.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
		pushOp("b.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest)),
	}
	present := map[destination]bool{
		{registry: "a.example.com", repository: "team/service-b", digest: manifestBDigest}: true,
	}
	plan, err := planDedupPush(dedupWorkingSet(ops, nil, dedupSelector{}, "", ""), present, "")
	if err != nil {
		t.Fatalf("planDedupPush: %v", err)
	}

	// Registry a: mounted out of the repository whose manifest it already serves, and
	// not mount-only, because this deploy did not put the blob there.
	if src := sourceIn(plan, "a.example.com", sharedLayerDigest); src.Repository != "team/service-b" || src.Registry != "a.example.com" {
		t.Errorf("source in a.example.com = %+v, want the present sibling team/service-b", src)
	}
	if mountOnlyIn(plan, "a.example.com", sharedLayerDigest) {
		t.Error("the shared layer is mount-only in a.example.com, but this deploy did not upload it there")
	}
	// Registry b: uploaded to its own home repository and mount-only there.
	if src := sourceIn(plan, "b.example.com", sharedLayerDigest); src.Repository != "team/service-a" || src.Registry != "b.example.com" {
		t.Errorf("source in b.example.com = %+v, want the home repository team/service-a", src)
	}
	if !mountOnlyIn(plan, "b.example.com", sharedLayerDigest) {
		t.Error("the shared layer is not mount-only in b.example.com, but this deploy uploaded it there")
	}
	if len(plan.uploads) != 1 {
		t.Errorf("plan uploads to %d repositories, want only b.example.com/team/service-a: %v", len(plan.uploads), plan.uploads)
	}
	if plan.confirmed != 1 || plan.mounted != 2 {
		t.Errorf("plan confirmed %d (registry, blob) pairs and %d cross-mounts, want 1 and 2", plan.confirmed, plan.mounted)
	}
}

// TestPlanDedupPushLeavesLoneRepositoriesAlone verifies that a registry where only one
// repository needs the blob is left alone even when another registry in the same
// deploy deduplicates it: there is nothing to mount it into there.
func TestPlanDedupPushLeavesLoneRepositoriesAlone(t *testing.T) {
	plan := planFor(t,
		pushOp("a.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
		pushOp("a.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest)),
		pushOp("b.example.com", "other/service-c", manifestADigest, layer(sharedLayerDigest)),
	)

	if src := sourceIn(plan, "a.example.com", sharedLayerDigest); src.Repository != "team/service-a" {
		t.Errorf("source in a.example.com = %+v, want the home repository team/service-a", src)
	}
	if src := sourceIn(plan, "b.example.com", sharedLayerDigest); src.Repository != "" {
		t.Errorf("source in b.example.com = %+v, want none: only one repository there needs the layer", src)
	}
	if len(plan.uploads) != 1 {
		t.Errorf("plan uploads to %d repositories, want only a.example.com/team/service-a: %v", len(plan.uploads), plan.uploads)
	}
	if plan.skipped != 1 {
		t.Errorf("plan left %d (registry, blob) pairs to the ordinary push, want 1", plan.skipped)
	}
}

// TestPlanDedupPushWithinDockerHub verifies that Docker Hub is not a special case: the
// mount go-containerregistry refuses to send there is the one whose source names a
// *different* registry (its issue 1741), and these sources always name the
// destination's own. Both spellings of Docker Hub are one registry.
func TestPlanDedupPushWithinDockerHub(t *testing.T) {
	plan := planFor(t,
		pushOp("docker.io", "acme/service-a", manifestADigest, layer(sharedLayerDigest)),
		pushOp("index.docker.io", "acme/service-b", manifestBDigest, layer(sharedLayerDigest)),
	)
	if src := sourceIn(plan, "index.docker.io", sharedLayerDigest); src.Repository != "acme/service-a" || src.Registry != "index.docker.io" {
		t.Errorf("source for the shared layer = %+v, want acme/service-a in index.docker.io", src)
	}
	if countSources(plan) != 1 {
		t.Errorf("plan recorded %d sources, want 1: the two spellings are one registry (%v)", countSources(plan), plan.sources)
	}
	if got := strings.Join(uploadedDigests(plan, "index.docker.io/acme/service-a"), ","); got != sharedLayerDigest {
		t.Errorf("uploads to acme/service-a = %v, want the shared layer", got)
	}

	// A base layer recorded as coming from Docker Hub under the other spelling is in
	// the destinations' registry, so it is mounted from there for free instead of
	// being uploaded to a home repository.
	base := layer(sharedLayerDigest, api.LayerSource{Registry: "docker.io", Repository: "library/base"})
	plan = planFor(t,
		pushOp("index.docker.io", "acme/service-a", manifestADigest, base),
		pushOp("index.docker.io", "acme/service-b", manifestBDigest, base),
	)
	if src := sourceIn(plan, "index.docker.io", sharedLayerDigest); src.Repository != "library/base" {
		t.Errorf("source for the base layer = %+v, want library/base", src)
	}
	if len(plan.uploads) != 0 || countMountOnly(plan) != 0 {
		t.Errorf("plan uploads %v / mount-only %v, want neither: the layer is already in library/base", plan.uploads, plan.mountOnly)
	}
	if plan.upstream != 1 {
		t.Errorf("plan mounts %d layers from upstream repositories, want 1", plan.upstream)
	}
}

// TestPlanDedupPushNeverCrossesRegistries pins the invariant that keeps the strategy
// usable on the many registries that do not implement cross-registry mounts: whatever
// the plan decides, a source recorded for a registry names that same registry.
func TestPlanDedupPushNeverCrossesRegistries(t *testing.T) {
	base := layer(sharedLayerDigest, api.LayerSource{Registry: "a.example.com", Repository: "library/base"})
	ops := []api.IndexedPushDeployOperation{
		pushOp("a.example.com", "team/service-a", manifestADigest, base),
		pushOp("a.example.com", "team/service-b", manifestBDigest, base),
		pushOp("b.example.com", "team/service-a", manifestADigest, base),
		pushOp("b.example.com", "team/service-b", manifestBDigest, base),
		pushOp("docker.io", "acme/service-a", manifestADigest, base),
		pushOp("docker.io", "acme/service-b", manifestBDigest, base),
	}
	present := map[destination]bool{
		{registry: "b.example.com", repository: "team/service-b", digest: manifestBDigest}: true,
	}
	plan, err := planDedupPush(dedupWorkingSet(ops, nil, dedupSelector{}, "", ""), present, "shared/blobs")
	if err != nil {
		t.Fatalf("planDedupPush: %v", err)
	}

	if countSources(plan) == 0 {
		t.Fatal("plan recorded no sources at all, so the invariant below is vacuous")
	}
	for registry, sources := range plan.sources {
		for digest, src := range sources {
			if src.Registry != registry {
				t.Errorf("source for %s in %s = %+v, want it to name %s", digest, registry, src, registry)
			}
		}
	}
	// Every upload target is in the registry it is planned for, too.
	for target := range plan.uploads {
		registry, _, found := strings.Cut(target, "/")
		if !found || plan.sources[registry] == nil {
			t.Errorf("upload target %q is not in a registry the plan mounts in", target)
		}
	}
}

// TestDedupWorkingSetAppliesOverrides verifies that --registry and --repository
// redirect the destinations the existence check asks about, and that operations
// writing the same manifest to the same place are merged into one destination.
func TestDedupWorkingSetAppliesOverrides(t *testing.T) {
	ops := []api.IndexedPushDeployOperation{
		pushOp("reg.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
		pushOp("other.example.com", "team/service-b", manifestADigest, layer(serviceALayerDiges)),
	}
	working := dedupWorkingSet(ops, nil, dedupSelector{}, "override.example.com", "override/repo")
	if len(working) != 1 {
		t.Fatalf("working set has %d entries, want the two operations merged into 1: %+v", len(working), working)
	}
	want := destination{registry: "override.example.com", repository: "override/repo", digest: manifestADigest}
	if working[0].dest != want {
		t.Errorf("destination = %+v, want %+v", working[0].dest, want)
	}
	if len(working[0].layers) != 2 {
		t.Errorf("merged destination needs %d layers, want both operations' layers", len(working[0].layers))
	}
}

// TestDedupWorkingSetIncludesRegistryTagOperations verifies registry_tag
// operations join the working set: they can name a repository their sibling push
// operation does not write to, and then their manifest's layers have to be
// mountable there too.
func TestDedupWorkingSetIncludesRegistryTagOperations(t *testing.T) {
	pushOps := []api.IndexedPushDeployOperation{
		pushOp("reg.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
	}
	tagOps := []api.IndexedRegistryTagDeployOperation{{
		RegistryTagDeployOperation: api.RegistryTagDeployOperation{
			BaseCommandOperation: api.BaseCommandOperation{
				Command:          "registry_tag",
				RootKind:         "manifest",
				DeduplicatedPush: true,
				Root:             api.Descriptor{Digest: manifestADigest},
				Manifests: []api.ManifestDeployInfo{{
					Descriptor: api.Descriptor{Digest: manifestADigest},
					LayerBlobs: []api.LayerBlob{layer(sharedLayerDigest)},
				}},
			},
			PushTarget: api.PushTarget{Registry: "reg.example.com", Repository: "team/mirror", Tags: []string{"stable"}},
		},
	}}

	working := dedupWorkingSet(pushOps, tagOps, dedupSelector{}, "", "")
	if len(working) != 2 {
		t.Fatalf("working set has %d entries, want the push and the registry_tag destination: %+v", len(working), working)
	}
	if working[1].dest.repository != "team/mirror" {
		t.Errorf("second destination repository = %q, want team/mirror", working[1].dest.repository)
	}

	// Both destinations need the layer, so it is deduplicated across them.
	plan, err := planDedupPush(working, nil, "")
	if err != nil {
		t.Fatalf("planDedupPush: %v", err)
	}
	if src := sourceIn(plan, testRegistry, sharedLayerDigest); src.Repository != "team/mirror" {
		t.Errorf("source for the shared layer = %+v, want team/mirror (first alphabetically)", src)
	}
}

// TestDedupWorkingSetLeavesOutIndexRoots verifies that an index root is not part
// of the working set: it references no blobs, so asking whether it exists would
// cost a request without pruning anything.
func TestDedupWorkingSetLeavesOutIndexRoots(t *testing.T) {
	indexDigest := "sha256:dddd444444444444444444444444444444444444444444444444444444444444"
	ops := []api.IndexedPushDeployOperation{indexPushOp("reg.example.com", "team/service-a", indexDigest)}

	working := dedupWorkingSet(ops, nil, dedupSelector{}, "", "")
	if len(working) != 2 {
		t.Fatalf("working set has %d entries, want one per child manifest: %+v", len(working), working)
	}
	for _, entry := range working {
		if entry.dest.digest == indexDigest {
			t.Errorf("index root %s is in the working set", indexDigest)
		}
	}
}

// indexPushOp builds an index push operation with two child manifests, each with
// its own layer.
func indexPushOp(registry, repository, indexDigest string) api.IndexedPushDeployOperation {
	return api.IndexedPushDeployOperation{
		PushDeployOperation: api.PushDeployOperation{
			BaseCommandOperation: api.BaseCommandOperation{
				Command:          "push",
				RootKind:         "index",
				DeduplicatedPush: true,
				Root:             api.Descriptor{MediaType: "application/vnd.oci.image.index.v1+json", Digest: indexDigest},
				Manifests: []api.ManifestDeployInfo{
					{Descriptor: api.Descriptor{Digest: manifestADigest}, LayerBlobs: []api.LayerBlob{layer(serviceALayerDiges)}},
					{Descriptor: api.Descriptor{Digest: manifestBDigest}, LayerBlobs: []api.LayerBlob{layer(serviceBLayerDiges)}},
				},
			},
			PushTarget: api.PushTarget{Registry: registry, Repository: repository},
		},
	}
}

// TestPlanDedupPushIndexPrunesPresentChildren verifies the per-manifest
// granularity pays off for a multi-platform image: a child manifest the registry
// already holds contributes no blobs even when its sibling still has to be pushed.
func TestPlanDedupPushIndexPrunesPresentChildren(t *testing.T) {
	indexDigest := "sha256:dddd444444444444444444444444444444444444444444444444444444444444"
	working := dedupWorkingSet([]api.IndexedPushDeployOperation{
		indexPushOp("reg.example.com", "team/service-a", indexDigest),
	}, nil, dedupSelector{}, "", "")
	present := map[destination]bool{
		{registry: "reg.example.com", repository: "team/service-a", digest: manifestADigest}: true,
	}
	plan, err := planDedupPush(working, present, "")
	if err != nil {
		t.Fatalf("planDedupPush: %v", err)
	}

	if plan.present != 1 {
		t.Errorf("plan counted %d manifests present, want 1", plan.present)
	}
	// Only the missing child's layer is still needed, by one repository, so nothing
	// is deduplicated.
	if plan.skipped != 1 {
		t.Errorf("plan left %d layers to the ordinary push, want only the missing child's", plan.skipped)
	}
}

// TestResolveBlobMount covers the per-registry decision: given the repositories in
// one registry that need a blob, the ones there whose manifests the registry already
// serves, and the repository there the build recorded the layer as coming from, where
// is the blob mounted from -- and does this deploy have to upload it first?
func TestResolveBlobMount(t *testing.T) {
	set := func(repositories ...string) map[string]struct{} {
		if len(repositories) == 0 {
			return nil
		}
		out := make(map[string]struct{}, len(repositories))
		for _, repository := range repositories {
			out[repository] = struct{}{}
		}
		return out
	}
	for _, tc := range []struct {
		name           string
		needing        map[string]struct{}
		confirmed      map[string]struct{}
		upstream       string
		blobRepository string
		want           blobMount
	}{
		{
			name:    "uploads to the first repository alphabetically",
			needing: set("team/c", "team/a", "team/b"),
			want:    blobMount{repository: "team/a", kind: mountFromUpload},
		},
		{
			name:    "a single repository is not worth it",
			needing: set("team/a"),
		},
		{
			name:    "no repository at all is not worth it",
			needing: set(),
		},
		{
			name:      "a repository the registry already serves needs no upload",
			needing:   set("team/b", "team/c"),
			confirmed: set("team/z"),
			want:      blobMount{repository: "team/z", kind: mountFromConfirmed},
		},
		{
			name:      "a confirmed repository beats uploading, even for a single destination",
			needing:   set("team/b"),
			confirmed: set("team/a"),
			want:      blobMount{repository: "team/a", kind: mountFromConfirmed},
		},
		{
			name:      "the first confirmed repository alphabetically",
			needing:   set("team/x", "team/y"),
			confirmed: set("team/c", "team/a"),
			want:      blobMount{repository: "team/a", kind: mountFromConfirmed},
		},
		{
			// The mount source is the only place the blob has to be: nothing to mount
			// it into, so this stays out of the strategy.
			name:      "a confirmed repository that is the only destination",
			needing:   set("team/a"),
			confirmed: set("team/a"),
		},
		{
			name:      "confirmed beats upstream",
			needing:   set("team/a", "team/b"),
			confirmed: set("team/z"),
			upstream:  "library/base",
			want:      blobMount{repository: "team/z", kind: mountFromConfirmed},
		},
		{
			name:     "upstream beats uploading",
			needing:  set("team/a", "team/b"),
			upstream: "library/base",
			want:     blobMount{repository: "library/base", kind: mountFromUpstream},
		},
		{
			name:           "upstream beats the staging repository",
			needing:        set("team/a", "team/b"),
			upstream:       "library/base",
			blobRepository: "shared/blobs",
			want:           blobMount{repository: "library/base", kind: mountFromUpstream},
		},
		{
			name:           "the staging repository beats uploading to a destination",
			needing:        set("team/a", "team/b"),
			blobRepository: "shared/blobs",
			want:           blobMount{repository: "shared/blobs", kind: mountFromUpload},
		},
		{
			// Unlike the destination-as-home case, staging is worth it even for a
			// single destination: the blob is already in the staging repository.
			name:           "the staging repository applies to a single destination too",
			needing:        set("team/a"),
			blobRepository: "shared/blobs",
			want:           blobMount{repository: "shared/blobs", kind: mountFromUpload},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveBlobMount(tc.needing, tc.confirmed, tc.upstream, tc.blobRepository); got != tc.want {
				t.Errorf("resolveBlobMount = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDedupSelector covers which operations deduplicate: each one's own setting,
// unless --deduplicated-push forces every operation or a sink turns the strategy
// off wholesale.
func TestDedupSelector(t *testing.T) {
	opted := api.BaseCommandOperation{DeduplicatedPush: true}
	plain := api.BaseCommandOperation{}
	for _, tc := range []struct {
		name      string
		override  string
		hasSink   bool
		wantOpted bool
		wantPlain bool
		wantErr   bool
	}{
		{name: "each operation decides", wantOpted: true, wantPlain: false},
		{name: "override turns every operation on", override: "enabled", wantOpted: true, wantPlain: true},
		{name: "override turns every operation off", override: "disabled", wantOpted: false, wantPlain: false},
		// A sink captures the operations locally, so there is nothing to
		// deduplicate: the setting is ignored rather than rejected, the same way a
		// sink already bypasses the push strategy and the staging repository.
		{name: "a sink turns it off", hasSink: true, wantOpted: false, wantPlain: false},
		{name: "a sink beats an explicit override", override: "enabled", hasSink: true, wantOpted: false, wantPlain: false},
		{name: "rejects a bogus override", override: "yes", wantErr: true},
		{name: "rejects a bogus override even with a sink", override: "yes", hasSink: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector, err := newDedupSelector(tc.override, tc.hasSink)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("newDedupSelector(%q) = (%+v, nil), want an error", tc.override, selector)
				}
				return
			}
			if err != nil {
				t.Fatalf("newDedupSelector: %v", err)
			}
			if got := selector.enabled(opted); got != tc.wantOpted {
				t.Errorf("enabled(opted-in operation) = %v, want %v", got, tc.wantOpted)
			}
			if got := selector.enabled(plain); got != tc.wantPlain {
				t.Errorf("enabled(opted-out operation) = %v, want %v", got, tc.wantPlain)
			}
			ops := []api.IndexedPushDeployOperation{
				{PushDeployOperation: api.PushDeployOperation{BaseCommandOperation: opted}},
				{PushDeployOperation: api.PushDeployOperation{BaseCommandOperation: plain}},
			}
			if got := selector.any(ops, nil); got != (tc.wantOpted || tc.wantPlain) {
				t.Errorf("any = %v, want %v", got, tc.wantOpted || tc.wantPlain)
			}
		})
	}
}

// TestDedupWorkingSetExcludesOptedOutOperations verifies that an operation which
// did not opt in contributes nothing: no destination to look up, and no layer to
// deduplicate. Its blobs must never be planned, because being handed a mount-only
// layer would fail its push on a registry that does not cross-mount blobs.
func TestDedupWorkingSetExcludesOptedOutOperations(t *testing.T) {
	optedOut := pushOp("reg.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest))
	optedOut.DeduplicatedPush = false
	ops := []api.IndexedPushDeployOperation{
		pushOp("reg.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
		optedOut,
	}

	working := dedupWorkingSet(ops, nil, dedupSelector{}, "", "")
	if len(working) != 1 {
		t.Fatalf("working set has %d entries, want only the opted-in operation: %+v", len(working), working)
	}
	if working[0].dest.repository != "team/service-a" {
		t.Errorf("destination repository = %q, want team/service-a", working[0].dest.repository)
	}

	// The shared layer now has a single deduplicating destination, so it is not
	// deduplicated at all -- and in particular is not mount-only, which would have
	// broken the opted-out operation's push.
	plan, err := planDedupPush(working, nil, "")
	if err != nil {
		t.Fatalf("planDedupPush: %v", err)
	}
	if countMountOnly(plan) != 0 || len(plan.uploads) != 0 {
		t.Errorf("plan mount-only %v / uploads %v, want neither", plan.mountOnly, plan.uploads)
	}
}

// TestStagingPushOperationsKeepsTheOptedOutOnes verifies the other half of a mixed
// deploy: the build-time blob staging flow (push_at_build_time_blob_repository) still
// applies to the operations the deduplicated push does not plan for. Dropping it for
// the whole deploy because one operation opted in would leave the others' layers
// nowhere -- staged in the blob repository they no longer point at.
func TestStagingPushOperationsKeepsTheOptedOutOnes(t *testing.T) {
	optedOut := pushOp("reg.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest))
	optedOut.DeduplicatedPush = false
	ops := []api.IndexedPushDeployOperation{
		pushOp("reg.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
		optedOut,
	}

	staging := stagingPushOperations(ops, dedupSelector{})
	if len(staging) != 1 || staging[0].Repository != "team/service-b" {
		t.Errorf("staging operations = %+v, want only team/service-b", staging)
	}

	// --deduplicated-push=disabled puts every operation back on the staging flow.
	if got := stagingPushOperations(ops, dedupSelector{override: deduplicatedPushDisabled}); len(got) != 2 {
		t.Errorf("staging operations with the strategy off = %d, want both", len(got))
	}
	// A sink does the same, since the strategy is off there too.
	if got := stagingPushOperations(ops, dedupSelector{off: true}); len(got) != 2 {
		t.Errorf("staging operations with a sink = %d, want both", len(got))
	}
	// ... and forcing it on takes them all off it.
	if got := stagingPushOperations(ops, dedupSelector{override: deduplicatedPushEnabled}); len(got) != 0 {
		t.Errorf("staging operations with the strategy forced on = %+v, want none", got)
	}
}

// TestPlanDedupPushMountsAcrossEveryOptedInOperation verifies the plan spans all
// the operations that opted in, not each one on its own: a layer three of their
// repositories need is uploaded once and cross-mounted into the other two, while
// the operation that opted out is left to upload its own copy.
func TestPlanDedupPushMountsAcrossEveryOptedInOperation(t *testing.T) {
	optedOut := pushOp("reg.example.com", "team/service-d", "sha256:dddd444444444444444444444444444444444444444444444444444444444444", layer(sharedLayerDigest))
	optedOut.DeduplicatedPush = false
	ops := []api.IndexedPushDeployOperation{
		pushOp("reg.example.com", "team/service-c", "sha256:cccc333333333333333333333333333333333333333333333333333333333333", layer(sharedLayerDigest)),
		pushOp("reg.example.com", "team/service-a", manifestADigest, layer(sharedLayerDigest)),
		pushOp("reg.example.com", "team/service-b", manifestBDigest, layer(sharedLayerDigest)),
		optedOut,
	}

	plan, err := planDedupPush(dedupWorkingSet(ops, nil, dedupSelector{}, "", ""), nil, "")
	if err != nil {
		t.Fatalf("planDedupPush: %v", err)
	}

	// One upload, to the first of the opted-in repositories alphabetically.
	if got := strings.Join(uploadedDigests(plan, "reg.example.com/team/service-a"), ","); got != sharedLayerDigest {
		t.Errorf("uploads to team/service-a = %v, want the shared layer", got)
	}
	if len(plan.uploads) != 1 {
		t.Errorf("plan uploads to %d repositories, want 1: %v", len(plan.uploads), plan.uploads)
	}
	// Cross-mounted into the other two opted-in repositories -- and only those: the
	// opted-out operation is not part of the plan.
	if plan.mounted != 2 {
		t.Errorf("plan cross-mounts %d times, want 2 (service-b and service-c)", plan.mounted)
	}
	if plan.total != 3 {
		t.Errorf("plan checked %d manifests, want the 3 opted-in ones", plan.total)
	}
}

func TestValidateDeduplicatedPush(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings api.DeploySettings
		wantErr  string
	}{
		{name: "needs no configuration at all", settings: api.DeploySettings{}},
		{
			name:     "rejects the bes strategy",
			settings: api.DeploySettings{PushStrategy: "bes"},
			wantErr:  "bes push strategy",
		},
		{
			name:     "rejects the cas_registry strategy",
			settings: api.DeploySettings{PushStrategy: "cas_registry"},
			wantErr:  "cas_registry push strategy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDeduplicatedPush(tc.settings)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateDeduplicatedPush: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateDeduplicatedPush = nil, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("validateDeduplicatedPush = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// TestUpstreamRepository covers the per-layer source lookup that decides whether a
// layer already lives in the destination registry.
func TestUpstreamRepository(t *testing.T) {
	sources := []api.LayerSource{
		{Registry: "other.example.com", Repository: "library/base"},
		{Registry: "reg.example.com", Repository: "mirror/base"},
	}
	if got := upstreamRepository(sources, "reg.example.com"); got != "mirror/base" {
		t.Errorf("upstreamRepository = %q, want mirror/base", got)
	}
	if got := upstreamRepository(sources, "third.example.com"); got != "" {
		t.Errorf("upstreamRepository for an unlisted registry = %q, want empty", got)
	}
	if got := upstreamRepository(nil, "reg.example.com"); got != "" {
		t.Errorf("upstreamRepository with no sources = %q, want empty", got)
	}
	// A source with no repository cannot be mounted from.
	if got := upstreamRepository([]api.LayerSource{{Registry: "reg.example.com"}}, "reg.example.com"); got != "" {
		t.Errorf("upstreamRepository for a source with no repository = %q, want empty", got)
	}
	// The destination registry arrives normalized, so the source's spelling must be
	// normalized too: these are all Docker Hub.
	for _, registry := range []string{"docker.io", "index.docker.io", ""} {
		source := []api.LayerSource{{Registry: registry, Repository: "library/base"}}
		if got := upstreamRepository(source, "index.docker.io"); got != "library/base" {
			t.Errorf("upstreamRepository for a source in %q = %q, want library/base", registry, got)
		}
	}
}

// TestDedupPlanUploadRepositoriesIsSorted keeps the upload order deterministic
// across runs.
func TestDedupPlanUploadRepositoriesIsSorted(t *testing.T) {
	plan := &dedupPlan{uploads: map[string][]registryv1.Hash{
		"reg.example.com/team/c": nil,
		"reg.example.com/team/a": nil,
		"reg.example.com/team/b": nil,
	}}
	got := strings.Join(plan.uploadRepositories(), ",")
	if got != "reg.example.com/team/a,reg.example.com/team/b,reg.example.com/team/c" {
		t.Errorf("uploadRepositories = %s, want them sorted", got)
	}
}
