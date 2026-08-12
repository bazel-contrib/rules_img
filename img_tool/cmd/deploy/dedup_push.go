package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/progress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// The deduplicated push strategy.
//
// go-containerregistry dedupes blob uploads per repository: every repository gets
// its own repoWriter, with its own "did I already upload this digest" bookkeeping.
// That is the right granularity for a registry whose blob store is shared across
// repositories -- the second upload is skipped after a cheap HEAD -- but the OCI
// spec allows a registry to keep a separate blob store per repository name, and
// some do. Pushing K images that share their base layers to K different
// repository names in such a registry uploads every shared layer K times.
//
// This is the upload-side counterpart to cas.CachingReader, which collapses the
// repeated *reads* of such a layer. They compose: a cross-mounted layer's bytes are
// never read, so this removes those reads rather than leaving them to the cache.
//
// When Settings.DeduplicatedPush is set, the deploy instead pushes in phases:
//
//  1. ask the registry which of the manifests it already holds, in parallel;
//  2. work out where each of the still-missing manifests' layers can be
//     cross-mounted from (see resolveBlobMount) -- often a repository that already
//     has it, because the registry just confirmed a manifest referencing it;
//  3. upload the ones with nowhere to mount from to their home repository -- once,
//     however many repositories need them;
//  4. serve them to the manifest push as mount-only layers, so the other
//     repositories cross-mount them out of the home repository and a request for
//     their bytes fails loudly instead of quietly re-uploading them.
//
// Phase 5 is the ordinary manifest push, unchanged. A blob headed for a single
// repository is left out of all of this, so a single-image deploy behaves exactly
// as it would with the strategy off.
//
// One deploy is the whole picture for the phases above, but not for the process: the
// persistent worker plans each work request on its own, so the home a blob is
// uploaded to is settled in a cache the whole process shares (dedup_locations.go).
// That is what keeps two requests sharing a layer from uploading it into a repository
// each -- and what makes the single-repository case above worth planning there.

// Values of the --deduplicated-push override. Empty inherits the deploy
// manifest's deduplicated_push setting.
const (
	deduplicatedPushEnabled  = "enabled"
	deduplicatedPushDisabled = "disabled"
)

// dedupSelector decides, per operation, whether its layers may be served by a
// cross-mount.
//
// The choice is per operation because it is an assumption about the destination: a
// deploy may push to a registry that cross-mounts blobs and to one that does not,
// and an operation that did not opt in must be pushed exactly as it would be
// without the strategy -- never handed a layer it is expected to mount.
type dedupSelector struct {
	// override is the --deduplicated-push value: "enabled" or "disabled" forces
	// every operation, "" leaves each one to its own setting.
	override string
	// off disables the strategy wholesale. A sink captures the operations locally,
	// so there is no registry to ask what it already holds and no repository to
	// cross-mount between: nothing to deduplicate. Turning it off is what --sink
	// already does to the rest of the push configuration -- it bypasses the push
	// strategy and the build-time staging repository as well -- so rejecting the
	// setting would be gratuitous friction for anyone who has it on in their
	// .bazelrc and wants to dump an image to a tarball.
	off bool
}

// newDedupSelector validates the --deduplicated-push override and returns the
// selector for it.
func newDedupSelector(override string, hasSink bool) (dedupSelector, error) {
	if err := validateDeduplicatedPushOverride(override); err != nil {
		return dedupSelector{}, err
	}
	return dedupSelector{override: override, off: hasSink}, nil
}

// enabled reports whether the given operation deduplicates.
func (s dedupSelector) enabled(op api.BaseCommandOperation) bool {
	if s.off {
		return false
	}
	switch s.override {
	case deduplicatedPushEnabled:
		return true
	case deduplicatedPushDisabled:
		return false
	}
	return op.DeduplicatedPush
}

// any reports whether any operation deduplicates, i.e. whether the preparation
// phases have anything to do.
func (s dedupSelector) any(pushOps []api.IndexedPushDeployOperation, tagOps []api.IndexedRegistryTagDeployOperation) bool {
	for _, op := range pushOps {
		if s.enabled(op.BaseCommandOperation) {
			return true
		}
	}
	for _, op := range tagOps {
		if s.enabled(op.BaseCommandOperation) {
			return true
		}
	}
	return false
}

// validateDeduplicatedPushOverride checks a --deduplicated-push value. Empty
// means "inherit the deploy manifest's setting".
func validateDeduplicatedPushOverride(value string) error {
	switch value {
	case "", deduplicatedPushEnabled, deduplicatedPushDisabled:
		return nil
	}
	return fmt.Errorf("invalid --deduplicated-push value %q: want %q or %q", value, deduplicatedPushEnabled, deduplicatedPushDisabled)
}

// validateDeduplicatedPush rejects configurations the strategy cannot serve. It
// runs before any registry I/O so a misconfiguration fails immediately rather
// than halfway through a deploy.
func validateDeduplicatedPush(settings api.DeploySettings) error {
	switch settings.PushStrategy {
	case "bes":
		return errors.New("deduplicated push is incompatible with the bes push strategy: the build event stream syncer performs the push, not this deploy")
	case "cas_registry":
		return errors.New("deduplicated push is incompatible with the cas_registry push strategy: blobs are committed to the CAS registry instead of being uploaded to the registry")
	}
	return nil
}

// dedupOptions configures a deduplicated push. The fields mirror the
// corresponding DeployOptions, except blobRepository, which is
// Settings.BlobRepository: when push at build time staged every blob there, it is
// where they are cross-mounted from (see resolveBlobMount). Empty -- the usual
// case -- means the mount source comes from the destinations themselves.
type dedupOptions struct {
	selector           dedupSelector
	blobRepository     string
	overrideRegistry   string
	overrideRepository string
	jobs               int
	// forbidUpload skips the deploy's own blob uploads: the blobs are expected to
	// already be in their home repository (Settings.ForbidLayerPush, e.g. because
	// they were pushed at build time).
	forbidUpload  bool
	pushTransport http.RoundTripper
	// locations is the process-wide blob location cache, shared by every deploy this
	// process plans (see dedup_locations.go). Nil plans this deploy on its own, which
	// is what the deploys that upload nothing do.
	locations *blobLocations
}

// destination is one manifest this deploy intends the registry to hold. Several
// operations may name the same destination (an extra tag on an already-pushed
// manifest, the same child manifest under two indexes), which is why it is
// comparable and used as a map key.
//
// registry is normalized (see normalizeRegistry), so two spellings of one registry
// are one destination -- and one key in every registry-keyed set derived from it.
type destination struct {
	registry   string
	repository string
	digest     string
}

func (d destination) String() string {
	return d.registry + "/" + d.repository + "@" + d.digest
}

func (d destination) digestRef() (name.Digest, error) {
	return name.NewDigest(d.registry+"/"+d.repository+"@"+d.digest, registryopts.NameOptions()...)
}

func (d destination) repositoryRef() repositoryRef {
	return repositoryRef{registry: d.registry, repository: d.repository}
}

// repositoryRef is a repository in a registry, without a manifest digest: the
// granularity blobs live at.
type repositoryRef struct {
	registry   string
	repository string
}

// manifestDestination pairs one manifest with the destination it is written to
// and the layers it needs. Pairing them keeps the existence check and the blob
// closure at the same granularity: a manifest the registry already holds
// contributes no layers, even when a sibling manifest of the same index still has
// to be pushed.
type manifestDestination struct {
	dest   destination
	layers []api.LayerBlob
}

// dedupWorkingSet flattens the deduplicating push and registry_tag operations into
// the manifests they intend the registry to hold. Operations that did not opt in
// are left out entirely: they contribute no destination to look up, no layer to
// deduplicate, and no repository to mount from or into.
//
// Index roots are left out too: they reference no blobs, so checking whether they
// exist would cost a request without pruning anything (the manifest push HEADs them
// anyway).
//
// registry_tag operations are included because their manifest must resolve through
// the same view as the push operation that created it: the tag is written by the
// same repoWriter machinery, so a layer it is expected to mount has to be recorded
// as mountable for it too. Their manifests and layers were ingested into the VFS by
// that sibling push. (The generator copies the sibling's registry and repository, so
// in practice they add no destination the push operations do not already contribute.)
func dedupWorkingSet(pushOps []api.IndexedPushDeployOperation, tagOps []api.IndexedRegistryTagDeployOperation, selector dedupSelector, overrideRegistry, overrideRepository string) []manifestDestination {
	var working []manifestDestination
	index := make(map[destination]int)

	add := func(registry, repository string, manifests []api.ManifestDeployInfo) {
		registry = normalizeRegistry(overrideOr(registry, overrideRegistry))
		repository = overrideOr(repository, overrideRepository)
		for _, manifest := range manifests {
			dest := destination{registry: registry, repository: repository, digest: manifest.Descriptor.Digest}
			if at, found := index[dest]; found {
				working[at].layers = append(working[at].layers, manifest.LayerBlobs...)
				continue
			}
			index[dest] = len(working)
			working = append(working, manifestDestination{dest: dest, layers: manifest.LayerBlobs})
		}
	}

	for _, op := range pushOps {
		if selector.enabled(op.BaseCommandOperation) {
			add(op.Registry, op.Repository, op.Manifests)
		}
	}
	for _, op := range tagOps {
		if selector.enabled(op.BaseCommandOperation) {
			add(op.Registry, op.Repository, op.Manifests)
		}
	}
	return working
}

// overrideOr returns override when it is set, else value: --registry and
// --repository replace an operation's destination when given.
func overrideOr(value, override string) string {
	if override != "" {
		return override
	}
	return value
}

// normalizeRegistry spells a registry the way go-containerregistry does, so that the
// two names of Docker Hub ("docker.io" and "index.docker.io") -- and an omitted
// registry, which also means Docker Hub -- are one registry here too. Registries are
// compared a lot in this file: to intersect the repositories available in each of
// them, to decide whether there is a single one to qualify a mount source with, and
// to key the destinations of the existence check.
//
// An unparseable registry is returned unchanged, so the error surfaces where the
// whole destination is parsed and can be named in the message.
func normalizeRegistry(registry string) string {
	parsed, err := name.NewRegistry(registry, registryopts.NameOptions()...)
	if err != nil {
		return registry
	}
	return parsed.RegistryStr()
}

// stagingPushOperations returns the push operations that keep the build-time blob
// staging flow (Settings.BlobRepository): the ones that did not opt into the
// deduplicated push, which plans its own mount sources and uploads instead. Gating
// this per operation is what lets one deploy mix the two -- an operation that opted
// out has to be pushed exactly as it would be with the strategy off, staging
// included.
func stagingPushOperations(ops []api.IndexedPushDeployOperation, selector dedupSelector) []api.IndexedPushDeployOperation {
	staging := make([]api.IndexedPushDeployOperation, 0, len(ops))
	for _, op := range ops {
		if selector.enabled(op.BaseCommandOperation) {
			continue
		}
		staging = append(staging, op)
	}
	return staging
}

// dedupPlan is what the planning phase decided: which blobs to upload where, and
// where every layer of the still-missing manifests is mounted from during the
// manifest push.
//
// Everything about a mount is decided per registry, because that is the scope a
// blob store and a cross-mount live in. The manifest push then sees one view per
// registry (see dedupViews), so the same layer can be mounted out of different
// repositories in different registries -- or uploaded in one and mounted in
// another.
type dedupPlan struct {
	// total is the number of manifests that were checked, present the number the
	// registry already holds at the expected digest. Present manifests contribute
	// no layers: a registry that has a manifest has the blobs it references.
	total   int
	present int
	// uploads maps a full target repository ("<registry>/<repository>") to the blob
	// digests to upload there, in a deterministic order. It is keyed by the whole
	// repository rather than by registry because two blobs in one registry can have
	// different home repositories.
	uploads map[string][]registryv1.Hash
	// joined lists the blobs whose home another request claimed first and is uploading
	// to. Their upload is in uploads too -- scheduled here rather than waited on -- but
	// it is another request's upload we are duplicating, so failing it only costs this
	// deploy the cross-mount (see uploadDedupBlobs).
	joined map[blobKey]struct{}
	// sources are the cross-mount sources for the layers of the still-missing
	// manifests, keyed by registry and then by digest string.
	sources map[string]map[string]api.CrossMountSource
	// mountOnly holds, per registry, the digests this deploy uploads to their home
	// repository there, so being asked for their bytes means the registry refused the
	// cross-mount.
	mountOnly map[string]map[string]struct{}
	// mounted counts the (registry, repository, blob) triples served by a cross-mount
	// rather than by an upload.
	mounted int
	// confirmed counts the (registry, blob) pairs mounted out of a repository whose
	// manifest the existence check found, and upstream those mounted out of a
	// repository the build recorded the layer as coming from. Neither needs an upload.
	confirmed int
	upstream  int
	// cached counts the (registry, blob) pairs whose home came out of the process-wide
	// location cache: a repository an earlier deploy in this process put the blob in,
	// or one a concurrent deploy is putting it in (those are in joined as well).
	cached int
	// waitedMu guards waited, which the upload phase counts from its goroutines.
	waitedMu sync.Mutex
	// waited counts the uploads another deploy of this process was already performing,
	// so this one waited for it and sent nothing (see blobLocations.uploadOnce).
	waited int
	// skipped counts the (registry, blob) pairs left to the ordinary push because
	// they have nowhere to be mounted from (see resolveBlobMount).
	skipped int
}

// countWaited records an upload another deploy of this process performed while this
// one waited for it.
func (p *dedupPlan) countWaited() {
	p.waitedMu.Lock()
	defer p.waitedMu.Unlock()
	p.waited++
}

// uploadRepositories returns the repositories with blobs to upload, sorted.
func (p *dedupPlan) uploadRepositories() []string {
	repositories := make([]string, 0, len(p.uploads))
	for repository := range p.uploads {
		repositories = append(repositories, repository)
	}
	slices.Sort(repositories)
	return repositories
}

// crossMountPlans returns the VFS view configuration for each registry the plan
// mounts anything in.
func (p *dedupPlan) crossMountPlans() map[string]deployvfs.CrossMountPlan {
	plans := make(map[string]deployvfs.CrossMountPlan, len(p.sources))
	for registry, sources := range p.sources {
		plans[registry] = deployvfs.CrossMountPlan{Sources: sources, MountOnly: p.mountOnly[registry]}
	}
	return plans
}

// report summarizes the plan on stderr, next to the blob-transfer stats the
// deploy prints. The counts are what the plan decided, so an upload
// go-containerregistry skipped after its own HEAD still shows up here.
func (p *dedupPlan) report(w io.Writer, opts dedupOptions) {
	uploaded := 0
	for _, digests := range p.uploads {
		uploaded += len(digests)
	}
	verb := "uploaded"
	if opts.forbidUpload {
		// Nothing was uploaded; the blobs were expected to be in place already.
		verb = "expected in place"
	}
	// The uploads a concurrent deploy of this process was already performing, which
	// this one waited for instead of repeating.
	var shared string
	if p.waited > 0 {
		shared = fmt.Sprintf(" (%d of them by a concurrent deploy in this process)", p.waited)
	}
	// Only mentioned when the process-wide cache contributed something, which it
	// cannot in a one-shot deploy planning every destination it has in one go.
	var fromCache string
	if p.cached > 0 {
		fromCache = fmt.Sprintf(", %d from a repository another deploy in this process filled (%d uploaded alongside it)", p.cached, len(p.joined))
	}
	fmt.Fprintf(w, "    deduplicated push: %d of %d manifests already present; %d blobs %s%s, %d mounted from a repository the registry serves, %d from an upstream repository%s; %d repository/blob pairs cross-mounted; %d layers left to the ordinary push\n",
		p.present, p.total, uploaded, verb, shared, p.confirmed, p.upstream, fromCache, p.mounted, p.skipped)
}

// blobKey is one blob in one registry: the granularity every mount decision is
// made at.
type blobKey struct {
	registry string
	digest   string
}

// planDedupPush decides where each layer is mounted from and which blobs have to
// be uploaded first. It performs no I/O: the registry's answers arrive as present.
//
// locations is the process-wide location cache, which is what lets deploys planned
// separately agree on one home per (registry, blob); it may be nil, and then this
// deploy is planned entirely on its own signals.
func planDedupPush(working []manifestDestination, present map[destination]bool, blobRepository string, locations *blobLocations) (*dedupPlan, error) {
	plan := &dedupPlan{
		total:     len(working),
		uploads:   make(map[string][]registryv1.Hash),
		joined:    make(map[blobKey]struct{}),
		sources:   make(map[string]map[string]api.CrossMountSource),
		mountOnly: make(map[string]map[string]struct{}),
	}

	// blobNeed accumulates, across every still-missing manifest in one registry that
	// references a blob, what we know about it.
	type blobNeed struct {
		// needing are the repositories in this registry that need the blob.
		needing map[string]struct{}
		// upstream is the repository that every needing destination agrees the blob
		// already lives in, from the per-layer sources recorded at build time. mixed
		// records that they disagree, which drops the upstream source: one source is
		// recorded per blob and registry, so it cannot say "mount from here for this
		// repository and from there for that one".
		upstream string
		mixed    bool
	}
	needs := make(map[blobKey]*blobNeed)
	var order []blobKey
	// A manifest the registry serves holds the blobs it references, so each of those
	// repositories is a mount source for them -- confirmed by the existence check a
	// moment ago, and free.
	confirmed := make(map[blobKey]map[string]struct{})

	for _, manifest := range working {
		if present[manifest.dest] {
			plan.present++
			for _, layer := range manifest.layers {
				key := blobKey{registry: manifest.dest.registry, digest: layer.Digest}
				if confirmed[key] == nil {
					confirmed[key] = make(map[string]struct{})
				}
				confirmed[key][manifest.dest.repository] = struct{}{}
			}
			continue
		}
		for _, layer := range manifest.layers {
			key := blobKey{registry: manifest.dest.registry, digest: layer.Digest}
			upstream := upstreamRepository(layer.Sources, manifest.dest.registry)
			need, found := needs[key]
			if !found {
				need = &blobNeed{needing: make(map[string]struct{}), upstream: upstream}
				needs[key] = need
				order = append(order, key)
			} else if upstream != need.upstream {
				need.mixed = true
			}
			need.needing[manifest.dest.repository] = struct{}{}
		}
	}

	for _, key := range order {
		need := needs[key]
		upstream := need.upstream
		if need.mixed {
			upstream = ""
		}
		resolution := locations.resolve(key, need.needing, confirmed[key], upstream, blobRepository)
		if resolution.repository == "" {
			// Nothing to gain: let the manifest push upload it as it normally would.
			plan.skipped++
			continue
		}

		// The source names the registry it is in, which is always the registry of the
		// destinations that need it -- see resolveBlobMount.
		if plan.sources[key.registry] == nil {
			plan.sources[key.registry] = make(map[string]api.CrossMountSource)
		}
		plan.sources[key.registry][key.digest] = api.CrossMountSource{Registry: key.registry, Repository: resolution.repository}
		mountedInto := countMountedInto(need.needing, resolution.repository)
		plan.mounted += mountedInto
		switch {
		case resolution.cached:
			plan.cached++
		case resolution.kind == mountFromConfirmed:
			plan.confirmed++
		case resolution.kind == mountFromUpstream:
			plan.upstream++
		}
		// A home this process puts the blob in is one we know the mount can come from,
		// which is what makes the layer mount-only -- whether the upload is this
		// request's own claim, one it joined, or one an earlier request already
		// finished.
		//
		// Except when this request mounts the blob nowhere: a home claimed for the one
		// repository that needs it (see resolveBlobMount's claimLone) is claimed for the
		// requests that come after, and they set their own mount-only. Insisting on it
		// here would turn "the upload landed but the registry cannot see it yet" into a
		// failed deploy, where uploading the bytes again is the right recovery.
		if resolution.kind == mountFromUpload && mountedInto > 0 {
			if plan.mountOnly[key.registry] == nil {
				plan.mountOnly[key.registry] = make(map[string]struct{})
			}
			plan.mountOnly[key.registry][key.digest] = struct{}{}
		}
		if resolution.upload {
			hash, err := registryv1.NewHash(key.digest)
			if err != nil {
				return nil, fmt.Errorf("parsing layer digest %s: %w", key.digest, err)
			}
			target := key.registry + "/" + resolution.repository
			plan.uploads[target] = append(plan.uploads[target], hash)
			if resolution.joined {
				plan.joined[key] = struct{}{}
			}
		}
	}
	// Whatever this deploy did not need, the next one in this process might: the
	// layers of the manifests the registry already serves are mountable out of those
	// repositories for free.
	locations.seedConfirmed(confirmed)

	for _, digests := range plan.uploads {
		slices.SortFunc(digests, func(a, b registryv1.Hash) int {
			return strings.Compare(a.String(), b.String())
		})
	}
	return plan, nil
}

// blobMount says where a blob is cross-mounted from during the manifest push. The
// registry is the caller's: see resolveBlobMount.
type blobMount struct {
	// repository is the mount source, or "" when the blob is not deduplicated and
	// is left to the ordinary manifest push.
	repository string
	kind       mountKind
}

// mountKind distinguishes the mount sources by how much we trust them, which is
// what decides whether the layer becomes mount-only.
type mountKind int

const (
	// mountFromConfirmed is a repository whose manifest the registry serves, so it
	// holds the blobs that manifest references.
	mountFromConfirmed mountKind = iota
	// mountFromUpstream is a repository the build recorded the layer as coming
	// from, e.g. a shallow base image's own repository.
	mountFromUpstream
	// mountFromUpload is a repository this deploy uploads the blob to itself, which
	// is what makes the layer mount-only.
	mountFromUpload
)

// resolveBlobMount decides where a blob is served from during the manifest push of
// one registry. Everything it is given is scoped to that registry: the repositories
// there that need the blob, the repositories there whose manifests the registry
// already serves, and the repository there the build recorded the layer as coming
// from. So is the answer -- which is what keeps every mount inside one registry,
// something few registries can do otherwise.
//
// The sources are tried in order of what they cost:
//
//  1. A repository whose manifest the registry already holds and which references
//     this blob. The existence check confirmed that manifest a moment ago, and a
//     registry that serves a manifest holds its blobs -- so the blob is already
//     there and nothing has to be uploaded at all. When one service out of many
//     changed, this covers every shared layer.
//  2. A repository the build recorded the layer as already living in (a shallow
//     base image's own repository).
//  3. blobRepository, when push at build time staged every blob there.
//  4. Otherwise one of the destinations themselves -- the lexicographically
//     smallest -- which this deploy uploads the blob to. No extra repository, and
//     no credentials beyond the ones the deploy already needs.
//
// Only (3) and (4) make the layer mount-only, because only there does this deploy
// put the blob in its mount source. (1) and (2) rest on an inference instead, so
// they keep the ordinary byte-upload fallback: if the blob turns out to be gone,
// uploading it is the right recovery rather than failing the deploy.
//
// Because the answer names a repository in the caller's registry, the recorded
// source can name that registry -- which matters more than it looks:
// go-containerregistry only asks the token service for read access to a mount
// source whose registry equals the destination's (scopesForUploadingImage and
// maybeUpdateScopes both compare them), so a registry-less source would be offered
// with a token that cannot read it. On a registry that enforces per-repository
// scopes that mount could not be authorized.
//
// One case reports no mount source: a blob whose only destination in this registry
// is the mount source itself. There is nothing to mount it into, so going through
// this at all would only move the upload out of the manifest push and cost an extra
// HEAD. This is what makes the strategy a no-op for a single-image deploy.
//
// claimLone overrides that last rule for a deploy whose destinations arrive
// incrementally (the persistent worker). There the blob's home is worth settling
// even with nothing to mount it into yet, because publishing it lets the *next* work
// request mount from it instead of uploading the blob into a repository of its own.
// The cost is one HEAD per layer: the upload moves into the deduplicated push's own
// phase, where go-containerregistry HEADs it, and the manifest push then finds the
// blob in its destination and skips it.
func resolveBlobMount(needing map[string]struct{}, confirmed map[string]struct{}, upstream, blobRepository string, claimLone bool) blobMount {
	if len(needing) == 0 {
		return blobMount{}
	}

	var mount blobMount
	switch home, found := smallestRepository(confirmed); {
	case found:
		mount = blobMount{repository: home, kind: mountFromConfirmed}
	case upstream != "":
		mount = blobMount{repository: upstream, kind: mountFromUpstream}
	case blobRepository != "":
		mount = blobMount{repository: blobRepository, kind: mountFromUpload}
	default:
		home, found := smallestRepository(needing)
		if !found {
			return blobMount{}
		}
		mount = blobMount{repository: home, kind: mountFromUpload}
	}

	if !claimLone && countMountedInto(needing, mount.repository) == 0 {
		return blobMount{}
	}
	return mount
}

// smallestRepository returns the lexicographically smallest repository of a set, so
// that a blob several repositories need always gets the same home whatever order
// the operations arrive in.
//
// Which is worth keeping even though the location cache already makes the deploys of
// one process agree on a home: two *processes* looking at the same registry agree
// too, so re-running a deploy -- or running the second half of one on another
// machine -- finds the blob in the home repository it would have picked and uploads
// nothing.
func smallestRepository(repositories map[string]struct{}) (string, bool) {
	if len(repositories) == 0 {
		return "", false
	}
	candidates := make([]string, 0, len(repositories))
	for repository := range repositories {
		candidates = append(candidates, repository)
	}
	return slices.Min(candidates), true
}

// countMountedInto returns how many of the needing repositories receive the blob by
// cross-mounting it from home (that is, all of them except home itself, which
// either holds the blob already or is where this deploy uploads it).
func countMountedInto(needing map[string]struct{}, home string) int {
	count := 0
	for repository := range needing {
		if repository != home {
			count++
		}
	}
	return count
}

// upstreamRepository returns the repository within registry that the layer is
// already available from, or "" if it records no source there. The sources are
// recorded per layer at build time (a layer pulled from a shallow base image
// carries the repository it came from), which makes this a much sharper signal
// than an operation's cross_mount_hint: that hint is attached to every layer of
// the operation, including locally built ones that exist nowhere in the registry.
//
// registry is a normalized destination registry (see destination), so the source's
// registry is normalized too before comparing: a base image recorded as coming from
// "docker.io" is in the same registry as a destination spelled "index.docker.io", and
// missing that would cost this layer a free mount and make it mount-only instead.
func upstreamRepository(sources []api.LayerSource, registry string) string {
	for _, source := range sources {
		if normalizeRegistry(source.Registry) == registry && source.Repository != "" {
			return source.Repository
		}
	}
	return ""
}

// findPresentManifests asks the registry which of the destinations it already
// holds at the expected digest, with --jobs requests in flight.
//
// A 404 means the manifest -- or the whole repository -- is absent. A 401 or 403
// means we cannot tell: some registries answer a manifest HEAD that way even when
// the caller may push (go-containerregistry's own existence check treats a 403
// the same way). "Cannot tell" has to mean "assume absent", because uploading a
// blob that turns out to be unnecessary only costs an upload, while skipping one
// that was needed produces a manifest the registry cannot serve. Any other error
// fails the deploy instead of silently degrading.
func findPresentManifests(ctx context.Context, dests []destination, jobs int, remoteOptions []remote.Option) (map[destination]bool, error) {
	if len(dests) == 0 {
		return nil, nil
	}
	puller, err := remote.NewPuller(remoteOptions...)
	if err != nil {
		return nil, fmt.Errorf("creating puller: %w", err)
	}

	found := make([]bool, len(dests))
	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(jobs)
	for i, dest := range dests {
		i, dest := i, dest
		g.Go(func() error {
			ref, err := dest.digestRef()
			if err != nil {
				return fmt.Errorf("parsing destination %s: %w", dest, err)
			}
			if _, err := puller.Head(groupCtx, ref); err != nil {
				if manifestAbsent(err) {
					return nil
				}
				return fmt.Errorf("checking whether %s is already present: %w", dest, err)
			}
			found[i] = true
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	present := make(map[destination]bool, len(dests))
	for i, dest := range dests {
		if found[i] {
			present[dest] = true
		}
	}
	return present, nil
}

// manifestAbsent reports whether err from a manifest HEAD means the manifest is
// absent, or that we are not in a position to tell.
func manifestAbsent(err error) bool {
	var terr *transport.Error
	if !errors.As(err, &terr) {
		return false
	}
	switch terr.StatusCode {
	case http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}

// uploadDedupBlobs uploads each blob the plan calls for to its home repository,
// with --jobs uploads in flight, and publishes each home in locations as its upload
// succeeds so the deploys that follow in this process mount from it instead.
//
// The blobs are read through VFS.RawLayer: the mount-only wrapping applies to the
// manifest push, not to the upload that puts the bytes in the home repository in
// the first place. go-containerregistry HEADs each blob before uploading it, so
// re-running a deploy costs one request per blob instead of a re-upload.
//
// Every upload goes through locations.uploadOnce, so a blob is never sent to the same
// repository twice at once, however many deploys of this process are pushing it:
// whichever of them gets there first uploads it while the others wait for that
// attempt. A deploy that waited transfers nothing and reads nothing -- the blob is not
// even resolved from the VFS, so a lazy push does not fetch it from the CAS.
//
// An upload this deploy claimed is its own work, so failing it fails the deploy. An
// upload it joined (plan.joined) is a duplicate of one another deploy in this process
// claimed, and failing that says nothing about this deploy's own access to the
// registry -- the home repository is one it never meant to write to. Rather than fail,
// it gives up the cross-mount for that blob: the layer stops being mount-only, so the
// manifest push mounts it if the claimer's upload made it and uploads the bytes into
// its own repository if it did not, which is what would have happened without the
// cache.
func uploadDedupBlobs(ctx context.Context, vfs *deployvfs.VFS, plan *dedupPlan, jobs int, remoteOptions []remote.Option, locations *blobLocations) (retErr error) {
	type upload struct {
		repo   name.Repository
		digest registryv1.Hash
		// key, home and target are the plan's own spelling of the destination, so that
		// what is published in locations is exactly what was claimed there. The parsed
		// repository above is not: it fills in the parts a registry client may leave out,
		// and "docker.io/nginx" comes back as the repository "library/nginx".
		key    blobKey
		home   string
		target uploadTarget
		joined bool
	}
	var uploads []upload
	for _, target := range plan.uploadRepositories() {
		repo, err := name.NewRepository(target, registryopts.NameOptions()...)
		if err != nil {
			return fmt.Errorf("parsing home repository %s: %w", target, err)
		}
		registry, home, _ := strings.Cut(target, "/")
		for _, digest := range plan.uploads[target] {
			key := blobKey{registry: registry, digest: digest.String()}
			_, joined := plan.joined[key]
			uploads = append(uploads, upload{
				repo:   repo,
				digest: digest,
				key:    key,
				home:   home,
				target: uploadTarget{registry: registry, repository: home, digest: digest.String()},
				joined: joined,
			})
		}
	}
	if len(uploads) == 0 {
		return nil
	}

	_, progCh, finishProgress := progress.TrackTransfers(ctx, "uploaded", "uploading shared blobs")
	pusher, err := remote.NewPusher(append(remoteOptions, remote.WithJobs(jobs), remote.WithProgress(progCh))...)
	if err != nil {
		finishProgress(err)
		return fmt.Errorf("creating pusher: %w", err)
	}
	defer func() { finishProgress(retErr) }()

	// The blobs whose joined upload failed, collected here and applied to the plan
	// once every upload has finished rather than from the goroutines themselves.
	var abandonedMu sync.Mutex
	var abandoned []upload

	g, groupCtx := errgroup.WithContext(ctx)
	g.SetLimit(jobs)
	for _, up := range uploads {
		up := up
		g.Go(func() error {
			mine := false
			err := locations.uploadOnce(groupCtx, up.target, func() error {
				mine = true
				layer, err := vfs.RawLayer(up.digest)
				if err != nil {
					return fmt.Errorf("resolving blob %s: %w", up.digest, err)
				}
				if err := pusher.Upload(groupCtx, up.repo, layer); err != nil {
					return fmt.Errorf("uploading blob %s to %s: %w", up.digest, up.repo, err)
				}
				return nil
			})
			switch {
			case err == nil:
				if !mine {
					// Another deploy in this process uploaded it while we waited.
					plan.countWaited()
				}
				locations.promote(up.key, up.home)
				return nil
			case !up.joined:
				// This deploy's own claim: the home is not where the blob is after all, so
				// give it up before failing, or the next deploy would mount from it.
				locations.abandon(up.key, up.home)
				return err
			case groupCtx.Err() != nil:
				// Cancelled because another upload failed; that error is the one to report.
				return nil
			default:
				abandonedMu.Lock()
				abandoned = append(abandoned, up)
				abandonedMu.Unlock()
				fmt.Fprintf(os.Stderr, "warning: leaving blob %s to the ordinary push: %v\n", up.digest, err)
				return nil
			}
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	for _, up := range abandoned {
		delete(plan.mountOnly[up.key.registry], up.key.digest)
	}
	return nil
}

// dedupViews serves the blobs of a deduplicated push. Each registry gets its own
// view of the VFS, because a mount source is a repository *in a registry*: the same
// layer can be mounted out of team/service-a in one registry, out of a shallow base
// image's repository in another, and uploaded in a third.
//
// A nil *dedupViews is usable and always answers with the plain VFS, so callers
// need no special case for "the strategy is off".
type dedupViews struct {
	plain            *deployvfs.VFS
	byRegistry       map[string]*deployvfs.VFS
	selector         dedupSelector
	overrideRegistry string
}

// For returns the VFS an operation's blobs are served from: the view of its
// destination registry when it opted in and that registry has mounts planned, else
// the plain VFS -- which is what keeps a mount-only layer away from an operation
// that did not opt in.
func (v *dedupViews) For(registry string, base api.BaseCommandOperation) *deployvfs.VFS {
	if v == nil {
		return nil
	}
	if !v.selector.enabled(base) {
		return v.plain
	}
	if view, found := v.byRegistry[normalizeRegistry(overrideOr(registry, v.overrideRegistry))]; found {
		return view
	}
	return v.plain
}

// prepareDedupPush runs the preparation phases of a deduplicated push and returns
// the views that the manifest push and the registry_tag operations must use. Each
// view shares its blobs, manifests and statistics with vfs, which keeps serving real
// bytes -- as the blob upload above and any concurrent load operation need it to.
func prepareDedupPush(ctx context.Context, vfs *deployvfs.VFS, pushOps []api.IndexedPushDeployOperation, tagOps []api.IndexedRegistryTagDeployOperation, opts dedupOptions) (*dedupViews, error) {
	if opts.jobs < 1 {
		// errgroup.SetLimit(0) would let no goroutine run at all.
		opts.jobs = 1
	}

	working := dedupWorkingSet(pushOps, tagOps, opts.selector, opts.overrideRegistry, opts.overrideRepository)
	dests := make([]destination, len(working))
	for i, manifest := range working {
		dests[i] = manifest.dest
	}
	remoteOptions := registryopts.Default().WithTransport(opts.pushTransport).WithJobs(opts.jobs).Remote()

	present, err := findPresentManifests(ctx, dests, opts.jobs, remoteOptions)
	if err != nil {
		return nil, err
	}

	// A deploy that uploads nothing learns nothing about where a blob is, and what it
	// would publish is an assumption about the registry it never checked: the blobs
	// are expected to be in place already. So it plans on its own signals and leaves
	// the process-wide cache untouched.
	locations := opts.locations
	if opts.forbidUpload {
		locations = nil
	}

	plan, err := planDedupPush(working, present, opts.blobRepository, locations)
	if err != nil {
		return nil, err
	}

	if !opts.forbidUpload {
		if err := uploadDedupBlobs(ctx, vfs, plan, opts.jobs, remoteOptions, locations); err != nil {
			return nil, fmt.Errorf("uploading shared blobs: %w", err)
		}
	}

	plan.report(os.Stderr, opts)
	views := &dedupViews{
		plain:            vfs,
		byRegistry:       make(map[string]*deployvfs.VFS),
		selector:         opts.selector,
		overrideRegistry: opts.overrideRegistry,
	}
	for registry, crossMountPlan := range plan.crossMountPlans() {
		views.byRegistry[registry] = vfs.WithCrossMountPlan(crossMountPlan)
	}
	return views, nil
}
