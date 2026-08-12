package deploy

import (
	"context"
	"sync"
)

// The process-wide blob location cache.
//
// planDedupPush plans one deploy: it knows every destination that deploy writes to,
// so it can pick one home repository per (registry, blob) and cross-mount the blob
// into the rest. In persistent worker mode that is not the whole picture. Each work
// request carries its own deploy manifest and is planned on its own, so two requests
// that share a layer would each pick a home among their own repositories and upload
// the layer there -- exactly the duplicate upload the strategy exists to remove, just
// spread across requests instead of across repositories.
//
// blobLocations is what makes those requests agree: one cache for the process,
// holding the repository this process has put a blob in, or is putting it in right
// now. The first request to need a blob in a registry claims a home for it and
// publishes it; every later request mounts from that same home instead of choosing
// its own.
//
// Keyed by (registry, digest) rather than by repository, because that is the scope a
// home is unique in: two requests must not pick different homes for the same blob in
// the same registry. Mounts still never cross registries -- every decision stays
// inside one registry, as it does within a single deploy.
//
// Nothing here survives the process. A restarted worker refills the cache from the
// registry itself: the existence check finds the manifests the previous process
// pushed, and their layers are mountable out of those repositories for free.

// blobLocations is the process-wide blob location cache described above. A nil
// *blobLocations is usable and remembers nothing, which is what the deploys that
// upload no blobs at all (Settings.ForbidLayerPush) pass.
//
// Every operation takes the same mutex for its whole decision, so concurrent requests
// resolving the same blob cannot create two homes for it: whoever gets the lock first
// claims, the rest observe that claim.
//
// It only ever grows, by one digest and one repository name per distinct blob the
// process pushes -- a few megabytes for a worker that pushes thousands of images, next
// to nothing beside the blobs themselves.
type blobLocations struct {
	mu sync.Mutex
	// present holds the homes this process knows to hold the blob: it uploaded it
	// there, or the registry confirmed a manifest referencing it in that repository.
	// The mount kind travels with the home, because it is what decides whether a
	// refused mount fails the push or falls back to uploading the bytes.
	present map[blobKey]blobMount
	// inflight holds the homes a request has claimed and is uploading to (or is about
	// to). A request that finds one here mounts from that home and schedules the same
	// upload itself rather than waiting for the claimer -- see resolve.
	inflight map[blobKey]blobMount
	// flights holds the uploads happening right now, so that two requests scheduling
	// the same one wait for it instead of sending the blob twice -- see uploadOnce.
	//
	// Keyed one level finer than the maps above: a home is picked per (registry, blob),
	// but an upload happens to a repository, and the same blob legitimately goes to a
	// different repository in another registry at the same time.
	flights map[uploadTarget]*uploadFlight
	// incremental records that more destinations may still arrive, i.e. that this is a
	// persistent worker rather than a one-shot deploy. It is what makes claiming a home
	// for a blob only one destination needs worth an upload phase: there is nothing to
	// cross-mount into today, but publishing the home lets the next work request mount
	// from it instead of uploading the blob into its own repository.
	incremental bool
}

// newBlobLocations returns an empty cache. incremental is true for the persistent
// worker, whose requests arrive one at a time, and false for a one-shot deploy,
// which plans every destination it will ever have in one go.
func newBlobLocations(incremental bool) *blobLocations {
	return &blobLocations{
		present:     make(map[blobKey]blobMount),
		inflight:    make(map[blobKey]blobMount),
		flights:     make(map[uploadTarget]*uploadFlight),
		incremental: incremental,
	}
}

// blobResolution is where a blob is mounted from during the manifest push, and what
// the request that asked has to do about it first.
type blobResolution struct {
	blobMount
	// upload records that this request has to upload the blob to blobMount.repository
	// before the manifest push mounts it from there.
	upload bool
	// joined records that another request claimed this home first and is uploading the
	// same blob to it. The upload above is then a duplicate scheduled so that this
	// request does not have to wait for a request it knows nothing about, and its
	// failure is not fatal (see uploadDedupBlobs).
	joined bool
	// cached records that the home came out of the cache rather than from this
	// request's own signals, which is what the report counts separately.
	cached bool
}

// resolve decides where a blob is served from, claiming a home for it in the cache
// when nothing knows one yet.
//
// The arguments are this request's own signals for one (registry, blob), all scoped
// to that registry the way resolveBlobMount needs them: the repositories that need
// the blob, the ones whose manifests the registry just confirmed, the repository the
// build recorded the layer as coming from, and the build-time staging repository.
//
// The cache is consulted first, and in the order the two maps cost:
//
//  1. A home in present needs nothing: the blob is there, so this request only
//     mounts it. When this process uploaded it, the home is mount-only, so a refused
//     mount fails the push rather than quietly uploading the blob again.
//  2. A home in inflight is one another request claimed and is uploading to. This
//     request mounts from the same home and schedules the same upload, which is what
//     keeps the phases local: no request ever waits on another request's completion,
//     and the mount source is in place by the end of *this* request's upload phase.
//     Each request uploads through a pusher of its own, so the worst case is the blob's
//     bytes crossing the wire twice -- but go-containerregistry HEADs a blob before
//     uploading it, so unless the two windows really overlap the second one costs a
//     request rather than a transfer, and either way the same repository ends up
//     holding it once.
//  3. Otherwise the request decides for itself (resolveBlobMount) and publishes the
//     answer: a home it uploads to is claimed in inflight until the upload succeeds,
//     anything else -- a repository the registry confirmed, an upstream repository --
//     goes straight into present for later requests to mount from.
func (l *blobLocations) resolve(key blobKey, needing, confirmed map[string]struct{}, upstream, blobRepository string) blobResolution {
	if l == nil {
		mount := resolveBlobMount(needing, confirmed, upstream, blobRepository, false)
		return blobResolution{blobMount: mount, upload: mount.kind == mountFromUpload}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if home, found := l.present[key]; found {
		return blobResolution{blobMount: home, cached: true}
	}
	if home, found := l.inflight[key]; found {
		return blobResolution{blobMount: home, upload: true, joined: true, cached: true}
	}

	mount := resolveBlobMount(needing, confirmed, upstream, blobRepository, l.incremental)
	switch {
	case mount.repository == "":
		return blobResolution{}
	case mount.kind == mountFromUpload:
		l.inflight[key] = mount
		return blobResolution{blobMount: mount, upload: true}
	default:
		l.present[key] = mount
		return blobResolution{blobMount: mount}
	}
}

// promote records that the blob is now in the home repository, so that the requests
// after this one mount it from there instead of uploading it again. It is called per
// blob as its upload succeeds, not once for the whole upload phase, so a request
// that fails halfway still publishes what it did put in place.
//
// The home is recorded as one this process uploaded to, which is the strongest of the
// mount kinds: the requests that mount from it fail loudly if the registry refuses,
// rather than quietly uploading the blob a second time.
func (l *blobLocations) promote(key blobKey, home string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.present[key] = blobMount{repository: home, kind: mountFromUpload}
	if inflight, found := l.inflight[key]; found && inflight.repository == home {
		delete(l.inflight, key)
	}
}

// abandon withdraws a claim whose upload failed, so that the next request picks a
// home of its own rather than mounting from a repository the blob never reached. The
// home is checked because the claim may have been withdrawn and re-made in the
// meantime; a claim another request has already promoted is left alone.
func (l *blobLocations) abandon(key blobKey, home string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if inflight, found := l.inflight[key]; found && inflight.repository == home {
		delete(l.inflight, key)
	}
}

// seedConfirmed publishes the repositories the existence check confirmed hold a
// blob, for the (registry, blob) pairs this request had no use for: the layers of a
// manifest the registry already serves, which this deploy therefore does not push
// anywhere. They cost nothing to remember and are exactly what a later work request
// pushing a sibling image needs -- a repository to mount the shared layers out of,
// with no upload and no HEAD of its own.
//
// Homes already in the cache are left alone: a claim in flight must not be replaced
// by a different repository, and a home this process uploaded to is no worse than a
// confirmed one.
func (l *blobLocations) seedConfirmed(confirmed map[blobKey]map[string]struct{}) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, repositories := range confirmed {
		if _, found := l.present[key]; found {
			continue
		}
		if _, found := l.inflight[key]; found {
			continue
		}
		// The smallest repository rather than an arbitrary one, so that two processes
		// looking at the same registry state agree on the home (see smallestRepository).
		home, found := smallestRepository(repositories)
		if !found {
			continue
		}
		l.present[key] = blobMount{repository: home, kind: mountFromConfirmed}
	}
}

// uploadTarget is one blob in one repository of one registry: the destination a blob
// upload has, and the granularity two uploads are the same piece of work at.
type uploadTarget struct {
	registry   string
	repository string
	digest     string
}

// uploadFlight is one upload of a blob to a repository that a goroutine somewhere in
// this process is performing right now. err is the attempt's result and is only read
// after done is closed, which the goroutine performing it does last.
type uploadFlight struct {
	done chan struct{}
	err  error
	// waiters counts the callers that found this flight and waited for it -- the
	// duplicate uploads it saved. Read and written under blobLocations.mu.
	waiters int
}

// uploadOnce uploads a blob to a repository at most once at a time for the whole
// process: whoever registers the flight first performs it, and everyone else waits
// for that attempt rather than sending the same bytes to the same place. Two
// requests that share a home are the reason it exists -- a joined upload (see
// resolve) is the same upload as the claimer's, and each request pushes through a
// remote.Pusher of its own, so go-containerregistry's own per-repository
// deduplication cannot see the other one.
//
// The wait is bounded by the attempt it waits for, and never by an attempt that may
// never happen: a flight only exists while a goroutine is inside upload, and the
// goroutine that registered it always publishes a result. ctx cuts the wait short if
// the caller gives up first, and only the caller's own ctx does -- the leader's
// cancellation surfaces as a failed attempt, which the waiter then retries with a
// context of its own rather than adopting an error from a request it has nothing to
// do with.
//
// On failure the waiter goes round: nobody is uploading the blob any more, so it
// registers a flight of its own and uploads it. Each caller therefore performs at
// most one attempt -- exactly what it would have done alone -- and the retries are
// bounded by the number of callers waiting on the same blob.
//
// Deadlock needs a cycle, and there is none to build: a goroutine holds no flight
// while waiting for another (uploadDedupBlobs gives each upload a goroutine of its
// own), and two goroutines of the same request never share a target, so a waiter's
// leader is always in another request and cannot be starved by the waiter's own
// concurrency limit.
func (l *blobLocations) uploadOnce(ctx context.Context, target uploadTarget, upload func() error) error {
	if l == nil {
		return upload()
	}
	for {
		l.mu.Lock()
		if flight, found := l.flights[target]; found {
			flight.waiters++
			l.mu.Unlock()
			select {
			case <-flight.done:
				if flight.err == nil {
					// Someone else uploaded the blob for us: it is in the repository, and no
					// bytes left this request at all.
					return nil
				}
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		flight := &uploadFlight{done: make(chan struct{})}
		l.flights[target] = flight
		l.mu.Unlock()

		// Publishing the result is the last thing this attempt does, and deferred so that
		// it happens however upload returns: a caller waiting for a flight that never
		// lands would wait for as long as its context allows. Withdrawing the flight
		// before closing done means a caller arriving after this starts an attempt of its
		// own rather than waiting for one that is already over.
		defer func() {
			l.mu.Lock()
			delete(l.flights, target)
			l.mu.Unlock()
			close(flight.done)
		}()
		flight.err = upload()
		return flight.err
	}
}
