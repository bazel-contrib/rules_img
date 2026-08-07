package registry

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registry"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// fakeCAS records what it was asked about and can be told what to answer.
type fakeCAS struct {
	batches [][]cas.Digest
	missing map[string]struct{}
	err     error
}

func (f *fakeCAS) FindMissingBlobs(_ context.Context, digests []cas.Digest) ([]cas.Digest, error) {
	f.batches = append(f.batches, digests)
	if f.err != nil {
		return nil, f.err
	}
	var missing []cas.Digest
	for _, digest := range digests {
		if _, ok := f.missing[hex.EncodeToString(digest.Hash)]; ok {
			missing = append(missing, digest)
		}
	}
	return missing, nil
}

// asked returns every digest the fake was asked about, flattened.
func (f *fakeCAS) asked() []string {
	var asked []string
	for _, batch := range f.batches {
		for _, digest := range batch {
			asked = append(asked, hex.EncodeToString(digest.Hash))
		}
	}
	return asked
}

type keepAliveFixture struct {
	clock     *fakeClock
	collector *registry.Collector
	store     registry.Store
	checker   *fakeCAS
	sizeCache *BlobSizeCache
	keepAlive *KeepAlive
}

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newKeepAliveFixture(t *testing.T, cfg KeepAliveConfig) *keepAliveFixture {
	t.Helper()
	// No collection TTL: the keepalive's job is to look after what the registry
	// still considers live, and most of these tests are about which of those
	// blobs are due a refresh.
	return newKeepAliveFixtureWithEviction(t, cfg, 0)
}

func newKeepAliveFixtureWithEviction(t *testing.T, cfg KeepAliveConfig, collectorTTL time.Duration) *keepAliveFixture {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
	store := registry.NewMemStore()
	collector := registry.NewCollector(store, registry.CollectorConfig{TTL: collectorTTL, Clock: clock.Now})
	checker := &fakeCAS{missing: map[string]struct{}{}}
	sizeCache := NewBlobSizeCache()

	cfg.Clock = clock.Now
	cfg.Log = log.New(io.Discard, "", 0)
	keepAlive, err := NewKeepAlive(collector, checker, sizeCache, cfg)
	if err != nil {
		t.Fatalf("NewKeepAlive: %v", err)
	}
	return &keepAliveFixture{
		clock:     clock,
		collector: collector,
		store:     store,
		checker:   checker,
		sizeCache: sizeCache,
		keepAlive: keepAlive,
	}
}

// addImage stores a manifest naming the given blobs and marks it live.
func (f *keepAliveFixture) addImage(t *testing.T, repo, name string, blobs ...registryv1.Descriptor) {
	t.Helper()
	manifest := imageManifestBytes(t, name, blobs...)
	digest := sha256Of(t, manifest)
	f.store.PutManifest(repo, digest, registry.Manifest{
		ContentType: string(types.OCIManifestSchema1),
		Kind:        registry.KindManifest,
		Blob:        manifest,
	})
	f.store.PutTag(repo, name, digest)
	f.collector.TouchManifest(repo, digest)
	f.collector.TouchTag(repo, name)
	for _, blob := range blobs {
		// The registry records a manifest's blobs as it stores it, so mirror
		// that here: a freshly pushed blob counts as freshly used.
		f.collector.TouchBlob(repo, blob.Digest, blob.Size)
		f.sizeCache.Set(blob.Digest, blob.Size)
	}
}

func TestKeepAliveRefreshesOnlyBlobsThatAreDue(t *testing.T) {
	// Two scan intervals of slack inside a one hour belief means a blob is
	// refreshed once it has gone 40 minutes without being used.
	fixture := newKeepAliveFixture(t, KeepAliveConfig{RemoteCacheTTL: time.Hour, ScanInterval: 10 * time.Minute})
	layer := descriptorFor(types.OCILayer, "layer")
	config := descriptorFor(types.OCIConfigJSON, "config")
	fixture.addImage(t, "app", "v1", config, layer)

	// Freshly pushed: nothing is anywhere near the remote cache's window.
	fixture.clock.advance(10 * time.Minute)
	stats, err := fixture.keepAlive.scanOnce(context.Background())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if stats.Live != 2 {
		t.Fatalf("scan saw %d live blobs, want 2", stats.Live)
	}
	if stats.Refreshed != 0 {
		t.Fatalf("scan refreshed %d blobs that were used 10m ago, want 0", stats.Refreshed)
	}
	if len(fixture.checker.batches) != 0 {
		t.Fatalf("scan made %d calls, want none", len(fixture.checker.batches))
	}

	// Past the refresh age, both blobs are asked about.
	fixture.clock.advance(31 * time.Minute)
	stats, err = fixture.keepAlive.scanOnce(context.Background())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if stats.Refreshed != 2 {
		t.Fatalf("scan refreshed %d blobs, want 2", stats.Refreshed)
	}
	asked := fixture.checker.asked()
	if len(asked) != 2 {
		t.Fatalf("scan asked about %v, want both blobs", asked)
	}

	// Having just refreshed them, the next scan has nothing to do.
	fixture.checker.batches = nil
	fixture.clock.advance(10 * time.Minute)
	stats, err = fixture.keepAlive.scanOnce(context.Background())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if stats.Refreshed != 0 {
		t.Fatalf("scan refreshed %d just-refreshed blobs, want 0", stats.Refreshed)
	}

	// And they come due again on the keepalive's own schedule, without the
	// registry having touched them.
	fixture.clock.advance(31 * time.Minute)
	stats, err = fixture.keepAlive.scanOnce(context.Background())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if stats.Refreshed != 2 {
		t.Fatalf("scan refreshed %d blobs on the second round, want 2", stats.Refreshed)
	}
}

func TestKeepAliveRefreshesEverythingWhenThereIsNoSlack(t *testing.T) {
	// Scanning as often as the belief allows leaves no room to wait, so every
	// live blob is refreshed on every scan.
	fixture := newKeepAliveFixture(t, KeepAliveConfig{RemoteCacheTTL: time.Hour, ScanInterval: time.Hour})
	fixture.addImage(t, "app", "v1", descriptorFor(types.OCILayer, "layer"))

	stats, err := fixture.keepAlive.scanOnce(context.Background())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if stats.Refreshed != 1 {
		t.Fatalf("scan refreshed %d blobs, want 1", stats.Refreshed)
	}
}

func TestKeepAliveBatchesLargeScans(t *testing.T) {
	fixture := newKeepAliveFixture(t, KeepAliveConfig{RemoteCacheTTL: time.Hour, ScanInterval: time.Hour})

	const blobs = keepAliveBatchSize + 7
	descriptors := make([]registryv1.Descriptor, 0, blobs)
	for i := range blobs {
		descriptors = append(descriptors, descriptorFor(types.OCILayer, "layer-"+strings.Repeat("x", i)))
	}
	fixture.addImage(t, "app", "v1", descriptors...)

	stats, err := fixture.keepAlive.scanOnce(context.Background())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if stats.Refreshed != blobs {
		t.Fatalf("scan refreshed %d blobs, want %d", stats.Refreshed, blobs)
	}
	if len(fixture.checker.batches) != 2 {
		t.Fatalf("scan made %d calls, want 2", len(fixture.checker.batches))
	}
	if len(fixture.checker.batches[0]) != keepAliveBatchSize {
		t.Fatalf("the first batch held %d digests, want %d", len(fixture.checker.batches[0]), keepAliveBatchSize)
	}
	if len(fixture.checker.batches[1]) != 7 {
		t.Fatalf("the second batch held %d digests, want 7", len(fixture.checker.batches[1]))
	}
}

func TestKeepAliveSkipsBlobsItCannotAskAbout(t *testing.T) {
	fixture := newKeepAliveFixture(t, KeepAliveConfig{RemoteCacheTTL: time.Hour, ScanInterval: time.Hour})
	// A descriptor with no size cannot be looked up: a CAS is keyed by digest
	// and size together.
	sizeless := registryv1.Descriptor{MediaType: types.OCILayer, Digest: descriptorFor(types.OCILayer, "layer").Digest}
	fixture.addImage(t, "app", "v1", sizeless)

	stats, err := fixture.keepAlive.scanOnce(context.Background())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if stats.Skipped != 1 || stats.Refreshed != 0 {
		t.Fatalf("scan got %+v, want 1 skipped and 0 refreshed", stats)
	}
	if len(fixture.checker.batches) != 0 {
		t.Fatalf("scan made %d calls for a blob it cannot address, want none", len(fixture.checker.batches))
	}
}

func TestKeepAliveReportsBlobsTheCacheLost(t *testing.T) {
	fixture := newKeepAliveFixture(t, KeepAliveConfig{RemoteCacheTTL: time.Hour, ScanInterval: time.Hour})
	layer := descriptorFor(types.OCILayer, "layer")
	kept := descriptorFor(types.OCILayer, "another layer")
	fixture.addImage(t, "app", "v1", layer, kept)
	fixture.checker.missing[layer.Digest.Hex] = struct{}{}

	stats, err := fixture.keepAlive.scanOnce(context.Background())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if stats.Missing != 1 {
		t.Fatalf("scan reported %d missing blobs, want 1", stats.Missing)
	}
	// A size we can no longer act on is worse than no size: drop it so requests
	// go back to asking the blob stores.
	if _, ok := fixture.sizeCache.Get(layer.Digest); ok {
		t.Fatal("a blob the cache lost kept its cached size")
	}
	if _, ok := fixture.sizeCache.Get(kept.Digest); !ok {
		t.Fatal("a blob the cache still has lost its cached size")
	}
}

func TestKeepAliveRetriesAfterAFailedCall(t *testing.T) {
	fixture := newKeepAliveFixture(t, KeepAliveConfig{RemoteCacheTTL: time.Hour, ScanInterval: time.Hour})
	fixture.addImage(t, "app", "v1", descriptorFor(types.OCILayer, "layer"))
	fixture.checker.err = errors.New("cache unavailable")

	if _, err := fixture.keepAlive.scanOnce(context.Background()); err == nil {
		t.Fatal("scanOnce hid a failed call")
	}

	// The failure must not count as a refresh, or the blob would be left to age
	// out until the next time it happens to come due.
	fixture.checker.err = nil
	fixture.checker.batches = nil
	stats, err := fixture.keepAlive.scanOnce(context.Background())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if stats.Refreshed != 1 {
		t.Fatalf("the scan after a failure refreshed %d blobs, want 1", stats.Refreshed)
	}
}

func TestKeepAliveForgetsBlobsThatAreNoLongerLive(t *testing.T) {
	fixture := newKeepAliveFixtureWithEviction(t,
		KeepAliveConfig{RemoteCacheTTL: time.Hour, ScanInterval: time.Hour},
		time.Minute,
	)
	layer := descriptorFor(types.OCILayer, "layer")
	fixture.addImage(t, "app", "v1", layer)

	if _, err := fixture.keepAlive.scanOnce(context.Background()); err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if len(fixture.keepAlive.lastKeptAlive) != 1 {
		t.Fatalf("the keepalive remembers %d blobs, want 1", len(fixture.keepAlive.lastKeptAlive))
	}

	// Untagging the only manifest that named it makes the blob nobody's
	// business, and once the registry collects it the keepalive's bookkeeping
	// goes with it.
	fixture.store.DeleteTag("app", "v1")
	fixture.collector.ForgetTag("app", "v1")
	fixture.clock.advance(2 * time.Minute)
	if stats := fixture.collector.Collect(); stats.Blobs != 1 {
		t.Fatalf("the registry collected %d blobs, want 1", stats.Blobs)
	}

	stats, err := fixture.keepAlive.scanOnce(context.Background())
	if err != nil {
		t.Fatalf("scanOnce: %v", err)
	}
	if stats.Live != 0 {
		t.Fatalf("scan saw %d live blobs after everything was collected, want 0", stats.Live)
	}
	if len(fixture.keepAlive.lastKeptAlive) != 0 {
		t.Fatalf("the keepalive still remembers %d blobs, want 0", len(fixture.keepAlive.lastKeptAlive))
	}
}

func TestNewKeepAliveRejectsUnusableConfigurations(t *testing.T) {
	store := registry.NewMemStore()
	collector := registry.NewCollector(store, registry.CollectorConfig{})
	checker := &fakeCAS{}
	valid := KeepAliveConfig{RemoteCacheTTL: time.Hour, ScanInterval: time.Minute, Log: log.New(io.Discard, "", 0)}

	for _, tc := range []struct {
		name      string
		collector *registry.Collector
		checker   BlobPresenceChecker
		cfg       KeepAliveConfig
	}{
		{"no collector", nil, checker, valid},
		{"no blob store", collector, nil, valid},
		{"no remote cache TTL", collector, checker, KeepAliveConfig{ScanInterval: time.Minute}},
		{"no scan interval", collector, checker, KeepAliveConfig{RemoteCacheTTL: time.Hour}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Log = log.New(io.Discard, "", 0)
			if _, err := NewKeepAlive(tc.collector, tc.checker, nil, tc.cfg); err == nil {
				t.Fatal("NewKeepAlive accepted an unusable configuration")
			}
		})
	}
}

func TestKeepAliveRunStopsWithItsContext(t *testing.T) {
	fixture := newKeepAliveFixture(t, KeepAliveConfig{RemoteCacheTTL: time.Hour, ScanInterval: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		fixture.keepAlive.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return when its context was cancelled")
	}
}

func descriptorFor(mediaType types.MediaType, content string) registryv1.Descriptor {
	digest, size, err := registryv1.SHA256(strings.NewReader(content))
	if err != nil {
		panic(err)
	}
	return registryv1.Descriptor{MediaType: mediaType, Digest: digest, Size: size}
}

func sha256Of(t *testing.T, blob []byte) registryv1.Hash {
	t.Helper()
	digest, _, err := registryv1.SHA256(strings.NewReader(string(blob)))
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	return digest
}

// imageManifestBytes serializes a manifest naming the given blobs: the first as
// its config, the rest as layers.
func imageManifestBytes(t *testing.T, name string, blobs ...registryv1.Descriptor) []byte {
	t.Helper()
	manifest := registryv1.Manifest{
		SchemaVersion: 2,
		MediaType:     types.OCIManifestSchema1,
		Layers:        []registryv1.Descriptor{},
		Annotations:   map[string]string{"test.name": name},
	}
	if len(blobs) > 0 {
		manifest.Config = blobs[0]
		manifest.Layers = blobs[1:]
	}
	blob, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	return blob
}
