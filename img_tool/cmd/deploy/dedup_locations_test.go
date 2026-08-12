package deploy

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These tests cover the process-wide blob location cache: the part of the
// deduplicated push that makes two deploys planned separately -- two work requests
// of the persistent worker -- upload a blob they share only once, and cross-mount it
// into the second one's repository instead.

// The registry and blob every test below resolves.
const locationsRegistry = "reg.example.com"

// repositorySet is the "repositories in this registry that need the blob" argument
// of blobLocations.resolve.
func repositorySet(repositories ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(repositories))
	for _, repository := range repositories {
		out[repository] = struct{}{}
	}
	return out
}

// sharedKey is the (registry, blob) pair the tests resolve.
func sharedKey() blobKey {
	return blobKey{registry: locationsRegistry, digest: sharedLayerDigest}
}

// TestBlobLocationsSequentialRequestsMountFromTheFirstHome is the sequential
// persistent worker: the first request to need a blob claims a home, uploads it and
// publishes it, so the next request -- which knows nothing about the first -- mounts
// it from there instead of uploading it into its own repository.
func TestBlobLocationsSequentialRequestsMountFromTheFirstHome(t *testing.T) {
	locations := newBlobLocations(true)
	key := sharedKey()

	// Request A, pushing team/service-a only. There is nothing to cross-mount into
	// yet, but the home is settled and published for whoever comes next.
	first := locations.resolve(key, repositorySet("team/service-a"), nil, "", "")
	if first.repository != "team/service-a" || first.kind != mountFromUpload {
		t.Fatalf("first resolution = %+v, want team/service-a as an uploaded home", first)
	}
	if !first.upload || first.joined || first.cached {
		t.Errorf("first resolution = %+v, want a fresh claim this request uploads", first)
	}
	locations.promote(key, first.repository)

	// Request B, pushing team/service-b: the blob is already in team/service-a, so it
	// is mounted from there and nothing is uploaded.
	second := locations.resolve(key, repositorySet("team/service-b"), nil, "", "")
	if second.repository != "team/service-a" {
		t.Errorf("second resolution = %+v, want the home the first request published", second)
	}
	if second.upload {
		t.Error("the second request uploads the blob, but the first one already put it in the home repository")
	}
	if !second.cached {
		t.Error("the second resolution is not reported as coming from the cache")
	}
	// This process put the blob there, so the layer is mount-only: a refused mount is
	// something to hear about rather than paper over with a full re-upload.
	if second.kind != mountFromUpload {
		t.Errorf("second resolution kind = %v, want the mount-only kind", second.kind)
	}
	if _, found := locations.inflight[key]; found {
		t.Error("the claim is still in flight after being promoted")
	}
}

// TestBlobLocationsConcurrentRequestsShareOneHome is the concurrent case: several
// requests resolve the same blob at once, each with a repository of its own. Exactly
// one of them claims the home; the rest mount from that same home and schedule the
// same upload rather than waiting for a request they know nothing about.
func TestBlobLocationsConcurrentRequestsShareOneHome(t *testing.T) {
	locations := newBlobLocations(true)
	key := sharedKey()

	const requests = 8
	resolutions := make([]blobResolution, requests)
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := range requests {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			resolutions[i] = locations.resolve(key, repositorySet(repositoryFor(i)), nil, "", "")
		}()
	}
	start.Done()
	done.Wait()

	claims := 0
	home := resolutions[0].repository
	for i, resolution := range resolutions {
		if resolution.repository != home {
			t.Fatalf("request %d resolved to %q, want the single home %q: concurrent requests must not pick two homes", i, resolution.repository, home)
		}
		if !resolution.upload {
			t.Errorf("request %d does not upload the blob, but no request has finished uploading it yet", i)
		}
		if !resolution.joined {
			claims++
		}
	}
	if claims != 1 {
		t.Errorf("%d requests claimed the home, want exactly 1 (the rest join its upload)", claims)
	}
	if !hasRepository(resolutions, home) {
		t.Errorf("home %q is not one of the requests' own repositories", home)
	}
	// Every one of them uploads to the home, so any of them succeeding is enough for
	// the mount, and the first to finish publishes it.
	locations.promote(key, home)
	if got := locations.resolve(key, repositorySet("team/late"), nil, "", ""); got.upload || got.repository != home {
		t.Errorf("a later request resolved to %+v, want a mount from %q with no upload", got, home)
	}
}

// repositoryFor names the repository the i-th concurrent request pushes to.
func repositoryFor(i int) string {
	return "team/service-" + string(rune('a'+i))
}

// hasRepository reports whether any resolution's own repository is home.
func hasRepository(resolutions []blobResolution, home string) bool {
	for i := range resolutions {
		if repositoryFor(i) == home {
			return true
		}
	}
	return false
}

// TestBlobLocationsAbandonedClaimIsClaimedAgain verifies that a claim whose upload
// failed is withdrawn: the next request picks a home of its own rather than mounting
// from a repository the blob never reached.
func TestBlobLocationsAbandonedClaimIsClaimedAgain(t *testing.T) {
	locations := newBlobLocations(true)
	key := sharedKey()

	first := locations.resolve(key, repositorySet("team/service-a"), nil, "", "")
	locations.abandon(key, first.repository)

	second := locations.resolve(key, repositorySet("team/service-b"), nil, "", "")
	if second.repository != "team/service-b" {
		t.Errorf("second resolution = %+v, want a home of its own after the first claim was abandoned", second)
	}
	if second.joined || !second.upload {
		t.Errorf("second resolution = %+v, want a fresh claim it uploads itself", second)
	}

	// A request that joined someone else's claim must not withdraw it when its own
	// duplicate upload fails: the claimer may still be uploading successfully.
	locations.abandon(key, "team/service-a")
	if home, found := locations.inflight[key]; !found || home.repository != "team/service-b" {
		t.Errorf("inflight home = %+v (found %v), want team/service-b to still own the claim", home, found)
	}
	// Nor may a promotion by the claimer be undone by a stale abandon.
	locations.promote(key, "team/service-b")
	locations.abandon(key, "team/service-b")
	if home, found := locations.present[key]; !found || home.repository != "team/service-b" {
		t.Errorf("present home = %+v (found %v), want the promoted home to survive", home, found)
	}
}

// TestBlobLocationsKeepsTheSofterSourcesSoft verifies that the cache carries the
// mount kind, not just the repository: a home this process only inferred -- a
// repository whose manifest the registry serves, an upstream repository -- keeps the
// ordinary byte-upload fallback for every request that mounts from it, while a home
// this process uploaded to is mount-only.
func TestBlobLocationsKeepsTheSofterSourcesSoft(t *testing.T) {
	for _, tc := range []struct {
		name      string
		confirmed map[string]struct{}
		upstream  string
		wantHome  string
		wantKind  mountKind
	}{
		{
			name:      "a repository the registry serves",
			confirmed: repositorySet("team/service-z"),
			wantHome:  "team/service-z",
			wantKind:  mountFromConfirmed,
		},
		{
			name:     "an upstream repository",
			upstream: "library/base",
			wantHome: "library/base",
			wantKind: mountFromUpstream,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			locations := newBlobLocations(true)
			key := sharedKey()

			first := locations.resolve(key, repositorySet("team/service-a"), tc.confirmed, tc.upstream, "")
			if first.repository != tc.wantHome || first.kind != tc.wantKind {
				t.Fatalf("first resolution = %+v, want %s as %v", first, tc.wantHome, tc.wantKind)
			}
			if first.upload {
				t.Error("the blob is uploaded, but it is already in the mount source")
			}
			// Published without an upload phase: nothing has to happen for the next
			// request to mount from the same place.
			second := locations.resolve(key, repositorySet("team/service-b"), nil, "", "")
			if second.repository != tc.wantHome || second.kind != tc.wantKind || second.upload {
				t.Errorf("second resolution = %+v, want the same %v mount with no upload", second, tc.wantKind)
			}
			if !second.cached {
				t.Error("the second resolution is not reported as coming from the cache")
			}
		})
	}
}

// TestBlobLocationsOneShotLeavesLoneDestinationsAlone verifies that a one-shot
// deploy still behaves as it did before there was a cache: a blob only one
// repository needs is left to the ordinary manifest push, because the plan is the
// whole picture and there is no later request to publish a home for.
func TestBlobLocationsOneShotLeavesLoneDestinationsAlone(t *testing.T) {
	locations := newBlobLocations(false)
	key := sharedKey()

	lone := locations.resolve(key, repositorySet("team/service-a"), nil, "", "")
	if lone.repository != "" || lone.upload {
		t.Errorf("resolution = %+v, want none: one destination has nothing to cross-mount into", lone)
	}
	if len(locations.inflight) != 0 || len(locations.present) != 0 {
		t.Errorf("cache recorded %v / %v, want nothing for a blob it did not plan", locations.present, locations.inflight)
	}

	// Two destinations are deduplicated as ever, and the home is published -- which a
	// one-shot deploy has no second plan to use, but costs nothing either.
	shared := locations.resolve(key, repositorySet("team/service-b", "team/service-a"), nil, "", "")
	if shared.repository != "team/service-a" || !shared.upload || shared.joined {
		t.Errorf("resolution = %+v, want the first repository alphabetically as a fresh claim", shared)
	}
}

// TestBlobLocationsSeedConfirmedPublishesUnusedRepositories verifies the free half
// of the cache: the layers of a manifest the registry already serves are recorded as
// available from that repository even though the deploy that checked has no use for
// them, because the next work request pushing a sibling image does.
func TestBlobLocationsSeedConfirmedPublishesUnusedRepositories(t *testing.T) {
	locations := newBlobLocations(true)
	key := sharedKey()
	other := blobKey{registry: locationsRegistry, digest: serviceALayerDiges}

	// A claim in flight must not be replaced by a confirmed repository: the requests
	// that joined it are uploading to the home it names.
	claimed := locations.resolve(other, repositorySet("team/service-a"), nil, "", "")
	locations.seedConfirmed(map[blobKey]map[string]struct{}{
		key:   repositorySet("team/service-z", "team/service-c"),
		other: repositorySet("team/service-z"),
	})

	if home, found := locations.present[key]; !found || home.repository != "team/service-c" {
		t.Errorf("seeded home = %+v (found %v), want the first confirmed repository alphabetically", home, found)
	}
	if home := locations.present[key]; home.kind != mountFromConfirmed {
		t.Errorf("seeded kind = %v, want the confirmed kind, which keeps the byte-upload fallback", home.kind)
	}
	if home, found := locations.inflight[other]; !found || home.repository != claimed.repository {
		t.Errorf("in-flight home = %+v (found %v), want the claim to survive seeding", home, found)
	}
	if _, found := locations.present[other]; found {
		t.Error("a blob with a claim in flight was also published as present")
	}
}

// TestBlobLocationsNilPlansOnItsOwn verifies the nil cache the deploys that upload
// nothing use (Settings.ForbidLayerPush): every operation works and every decision
// comes from the request's own signals.
func TestBlobLocationsNilPlansOnItsOwn(t *testing.T) {
	var locations *blobLocations
	key := sharedKey()

	first := locations.resolve(key, repositorySet("team/service-b", "team/service-a"), nil, "", "")
	if first.repository != "team/service-a" || !first.upload || first.cached {
		t.Errorf("resolution = %+v, want the plain per-deploy answer", first)
	}
	// Nothing is remembered, so a second deploy decides for itself again.
	locations.promote(key, first.repository)
	second := locations.resolve(key, repositorySet("team/service-c"), nil, "", "")
	if second.repository != "" {
		t.Errorf("resolution = %+v, want none: a nil cache remembers nothing", second)
	}
	locations.abandon(key, first.repository)
	locations.seedConfirmed(map[blobKey]map[string]struct{}{key: repositorySet("team/service-z")})

	// An upload still happens, it is just never deduplicated against anything.
	uploads := 0
	if err := locations.uploadOnce(context.Background(), sharedTarget("team/service-a"), func() error {
		uploads++
		return nil
	}); err != nil || uploads != 1 {
		t.Errorf("uploadOnce = %v after %d uploads, want one upload and no error", err, uploads)
	}
}

// sharedTarget is the upload destination the flight tests use: the shared blob in a
// repository of the test registry.
func sharedTarget(repository string) uploadTarget {
	return uploadTarget{registry: locationsRegistry, repository: repository, digest: sharedLayerDigest}
}

// waitForWaiters blocks until at least want callers are waiting for the flight to
// target, so a test can be sure the callers piled up behind one attempt rather than
// arriving one after another.
func waitForWaiters(t *testing.T, locations *blobLocations, target uploadTarget, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		locations.mu.Lock()
		got := 0
		if flight, found := locations.flights[target]; found {
			got = flight.waiters
		}
		locations.mu.Unlock()
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d callers are waiting for the upload of %+v, want %d: they are not sharing one attempt", got, target, want)
		}
		time.Sleep(50 * time.Microsecond)
	}
}

// TestUploadOnceUploadsABlobToARepositoryOnceAtATime is what the flights exist for:
// several deploys of this process scheduling the same upload must not send the same
// blob to the same repository at the same time. Whichever gets there first performs
// it; the rest wait for that attempt and send nothing.
//
// Each deploy pushes through a remote.Pusher of its own, so go-containerregistry's
// per-repository deduplication cannot see the others -- without this they would all
// HEAD the blob, all miss, and all upload it.
func TestUploadOnceUploadsABlobToARepositoryOnceAtATime(t *testing.T) {
	locations := newBlobLocations(true)
	target := sharedTarget("team/service-a")

	const callers = 8
	var attempts, concurrent atomic.Int32
	inUpload := make(chan struct{})
	release := make(chan struct{})
	var announce sync.Once
	upload := func() error {
		attempts.Add(1)
		if concurrent.Add(1) > 1 {
			t.Error("two goroutines uploaded the same blob to the same repository at once")
		}
		defer concurrent.Add(-1)
		announce.Do(func() { close(inUpload) })
		<-release
		return nil
	}

	errs := make([]error, callers)
	var done sync.WaitGroup
	for i := range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			errs[i] = locations.uploadOnce(context.Background(), target, upload)
		}()
	}
	// An attempt is in flight, and every other caller is behind it rather than sending
	// the blob a second time.
	<-inUpload
	waitForWaiters(t, locations, target, callers-1)
	close(release)
	done.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("the blob was uploaded %d times for %d callers, want exactly 1", got, callers)
	}
	if len(locations.flights) != 0 {
		t.Errorf("%d flights are still registered, want none once every upload has finished", len(locations.flights))
	}
}

// TestUploadOnceRetriesAfterAFailedAttempt verifies the failure half of the contract:
// a caller that waited for an attempt which failed is not handed that failure, because
// the blob still has to get there. It takes the upload over itself, and only its own
// attempt decides its result.
//
// Which is also what keeps one work request's cancellation out of another's: a
// cancelled upload is just a failed attempt to whoever was waiting for it.
func TestUploadOnceRetriesAfterAFailedAttempt(t *testing.T) {
	locations := newBlobLocations(true)
	target := sharedTarget("team/service-a")
	failure := errors.New("upload refused")

	var attempts atomic.Int32
	inUpload := make(chan struct{})
	release := make(chan struct{})
	leaderErr := make(chan error, 1)
	go func() {
		leaderErr <- locations.uploadOnce(context.Background(), target, func() error {
			attempts.Add(1)
			close(inUpload)
			<-release
			return failure
		})
	}()
	<-inUpload

	waiterErr := make(chan error, 1)
	go func() {
		waiterErr <- locations.uploadOnce(context.Background(), target, func() error {
			attempts.Add(1)
			return nil
		})
	}()
	close(release)

	if err := <-leaderErr; !errors.Is(err, failure) {
		t.Errorf("the failed attempt returned %v, wants its own error %v", err, failure)
	}
	if err := <-waiterErr; err != nil {
		t.Errorf("the waiting caller returned %v, want it to have retried and succeeded", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("the blob was uploaded %d times, want 2: the failed attempt and the retry", got)
	}
}

// TestUploadOnceSerializesNothingElse verifies the flights are per destination: two
// uploads that are not the same piece of work run at the same time, even when they
// are the same blob in a different repository -- a registry with per-repository blob
// storage needs both.
func TestUploadOnceSerializesNothingElse(t *testing.T) {
	locations := newBlobLocations(true)
	targets := []uploadTarget{
		sharedTarget("team/service-a"),
		sharedTarget("team/service-b"),
		{registry: locationsRegistry, repository: "team/service-a", digest: serviceALayerDiges},
		{registry: "other.example.com", repository: "team/service-a", digest: sharedLayerDigest},
	}

	// Each upload waits for every other one to have started, so anything serialized
	// here would never finish.
	var inUpload sync.WaitGroup
	inUpload.Add(len(targets))
	var done sync.WaitGroup
	finished := make(chan struct{})
	for _, target := range targets {
		done.Add(1)
		go func() {
			defer done.Done()
			if err := locations.uploadOnce(context.Background(), target, func() error {
				inUpload.Done()
				inUpload.Wait()
				return nil
			}); err != nil {
				t.Errorf("uploading %+v: %v", target, err)
			}
		}()
	}
	go func() { done.Wait(); close(finished) }()

	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("uploads to different destinations were serialized: they never all ran at once")
	}
}

// TestUploadOnceStopsWaitingWhenTheCallerGivesUp verifies a caller is never stuck
// behind another request's upload: its own context ends the wait, and it reports that
// rather than the other request's outcome.
func TestUploadOnceStopsWaitingWhenTheCallerGivesUp(t *testing.T) {
	locations := newBlobLocations(true)
	target := sharedTarget("team/service-a")

	inUpload := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	leaderErr := make(chan error, 1)
	go func() {
		leaderErr <- locations.uploadOnce(context.Background(), target, func() error {
			close(inUpload)
			<-release
			return nil
		})
	}()
	<-inUpload

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := locations.uploadOnce(ctx, target, func() error {
		t.Error("the caller uploaded the blob although its context was already cancelled")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("uploadOnce = %v, want the caller's own context error", err)
	}
}
