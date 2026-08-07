package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// testRegistry is a registry served in process, with a clock the test moves.
type testRegistry struct {
	t         *testing.T
	handler   http.Handler
	store     Store
	collector *Collector
	clock     *testClock
}

func newTestRegistry(t *testing.T, cfg CollectorConfig, opts ...Option) *testRegistry {
	t.Helper()
	clock := newTestClock()
	cfg.Clock = clock.Now
	store := NewMemStore()
	collector := NewCollector(store, cfg)
	opts = append([]Option{
		WithStore(store),
		WithCollector(collector),
		WithReferrersSupport(true),
		Logger(log.New(io.Discard, "", 0)),
	}, opts...)
	return &testRegistry{t: t, handler: New(opts...), store: store, collector: collector, clock: clock}
}

func newTestRegistryWithoutEviction(t *testing.T, opts ...Option) *testRegistry {
	t.Helper()
	store := NewMemStore()
	opts = append([]Option{
		WithStore(store),
		WithReferrersSupport(true),
		Logger(log.New(io.Discard, "", 0)),
	}, opts...)
	return &testRegistry{t: t, handler: New(opts...), store: store, clock: newTestClock()}
}

func (r *testRegistry) do(method, path string, body []byte, header http.Header) *httptest.ResponseRecorder {
	r.t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for name, values := range header {
		req.Header[name] = values
	}
	recorder := httptest.NewRecorder()
	r.handler.ServeHTTP(recorder, req)
	return recorder
}

// putManifest pushes a manifest under ref, which is either a tag or the
// manifest's own digest, and returns its digest.
func (r *testRegistry) putManifest(repo, ref string, mediaType types.MediaType, blob []byte) v1.Hash {
	r.t.Helper()
	header := http.Header{"Content-Type": []string{string(mediaType)}}
	recorder := r.do(http.MethodPut, "/v2/"+repo+"/manifests/"+ref, blob, header)
	if recorder.Code != http.StatusCreated {
		r.t.Fatalf("PUT %s/manifests/%s got %d and body %q, want 201", repo, ref, recorder.Code, recorder.Body)
	}
	return digestOf(r.t, blob)
}

// putImage pushes a manifest by digest, the way a client pushes the layers of a
// multi-platform image before pushing the index over them.
func (r *testRegistry) putImage(repo string, mediaType types.MediaType, blob []byte) v1.Hash {
	r.t.Helper()
	return r.putManifest(repo, digestOf(r.t, blob).String(), mediaType, blob)
}

func (r *testRegistry) assertManifestStatus(repo, ref string, want int) {
	r.t.Helper()
	recorder := r.do(http.MethodGet, "/v2/"+repo+"/manifests/"+ref, nil, nil)
	if recorder.Code != want {
		r.t.Fatalf("GET %s/manifests/%s got %d and body %q, want %d", repo, ref, recorder.Code, recorder.Body, want)
	}
}

func (r *testRegistry) assertManifestBody(repo, ref string, want []byte) {
	r.t.Helper()
	recorder := r.do(http.MethodGet, "/v2/"+repo+"/manifests/"+ref, nil, nil)
	if recorder.Code != http.StatusOK {
		r.t.Fatalf("GET %s/manifests/%s got %d and body %q, want 200", repo, ref, recorder.Code, recorder.Body)
	}
	if !bytes.Equal(recorder.Body.Bytes(), want) {
		r.t.Fatalf("GET %s/manifests/%s served %q, want %q", repo, ref, recorder.Body, want)
	}
	if got, want := recorder.Header().Get("Docker-Content-Digest"), digestOf(r.t, want).String(); got != want {
		r.t.Fatalf("GET %s/manifests/%s reported digest %s, want %s", repo, ref, got, want)
	}
}

// multiPlatformImage is the shape a rules_img push takes: a per-platform
// manifest, then an index over it, then a tag.
type multiPlatformImage struct {
	config, layer v1.Descriptor
	child         []byte
	childDigest   v1.Hash
	index         []byte
	indexDigest   v1.Hash
}

func newMultiPlatformImage(t *testing.T) multiPlatformImage {
	t.Helper()
	config := descriptorFor(types.OCIConfigJSON, "config")
	layer := descriptorFor(types.OCILayer, "layer")
	child := imageManifest(t, "child", config, layer)
	index := imageIndex(t, "index", manifestDescriptor(child, types.OCIManifestSchema1))
	return multiPlatformImage{
		config:      config,
		layer:       layer,
		child:       child,
		childDigest: digestOf(t, child),
		index:       index,
		indexDigest: digestOf(t, index),
	}
}

// TestRegistryTagRefreshKeepsChildManifestsPullable covers the first failure
// mode in https://github.com/bazel-contrib/rules_img/issues/695: an index kept
// alive by a second tag has to keep the children it names alive with it, or the
// index resolves and pulling it fails on a child that is gone.
func TestRegistryTagRefreshKeepsChildManifestsPullable(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute, TagTTL: time.Minute})
	image := newMultiPlatformImage(t)

	reg.putImage("example/app", types.OCIManifestSchema1, image.child)
	reg.putImage("example/app", types.OCIImageIndex, image.index)
	reg.putManifest("example/app", "initial", types.OCIImageIndex, image.index)

	// A later pipeline publishes another tag for the same index. Nothing
	// re-pushes the child.
	reg.clock.advance(30 * time.Second)
	reg.putManifest("example/app", "tag-1", types.OCIImageIndex, image.index)

	// Past the child's own expiry: the index is still tagged, so the child it
	// names is still there.
	reg.clock.advance(31 * time.Second)
	reg.assertManifestBody("example/app", "tag-1", image.index)
	reg.assertManifestBody("example/app", image.indexDigest.String(), image.index)
	reg.assertManifestBody("example/app", image.childDigest.String(), image.child)

	// The first tag expired on its own schedule, which is the whole point of a
	// tag TTL, and took nothing else with it.
	reg.assertManifestStatus("example/app", "initial", http.StatusNotFound)
}

// TestRegistryTaggingARefreshedIndexAgainSucceeds covers the second failure mode
// in issue #695: a publisher that finds the index already present skips pushing
// its children, and the tag PUT then has to validate against children that must
// still be there.
func TestRegistryTaggingARefreshedIndexAgainSucceeds(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute, TagTTL: time.Minute})
	image := newMultiPlatformImage(t)

	reg.putImage("example/app", types.OCIManifestSchema1, image.child)
	reg.putImage("example/app", types.OCIImageIndex, image.index)
	reg.putManifest("example/app", "initial", types.OCIImageIndex, image.index)

	reg.clock.advance(30 * time.Second)
	// A HEAD is how a publisher decides the index is already there.
	if recorder := reg.do(http.MethodHead, "/v2/example/app/manifests/"+image.indexDigest.String(), nil, nil); recorder.Code != http.StatusOK {
		t.Fatalf("HEAD of an existing index got %d, want 200", recorder.Code)
	}
	reg.putManifest("example/app", "tag-1", types.OCIImageIndex, image.index)

	// Past the child's original expiry, pushing another tag for that index must
	// still pass the sub-manifest check.
	reg.clock.advance(31 * time.Second)
	reg.putManifest("example/app", "tag-2", types.OCIImageIndex, image.index)
	reg.assertManifestBody("example/app", "tag-2", image.index)
}

func TestRegistryTagIsAPointerNotACopy(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute})
	first := imageManifest(t, "first", descriptorFor(types.OCIConfigJSON, "config"))
	second := imageManifest(t, "second", descriptorFor(types.OCIConfigJSON, "config"))

	oldDigest := reg.putManifest("app", "stable", types.OCIManifestSchema1, first)
	reg.clock.advance(30 * time.Second)
	newDigest := reg.putManifest("app", "stable", types.OCIManifestSchema1, second)

	// The tag now points at the new manifest and, being a tag, keeps it. The
	// manifest it used to point at is nobody's business any more.
	reg.clock.advance(31 * time.Second)
	reg.assertManifestBody("app", "stable", second)
	reg.assertManifestBody("app", newDigest.String(), second)
	reg.assertManifestStatus("app", oldDigest.String(), http.StatusNotFound)
}

func TestRegistryUntaggedManifestExpires(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute})
	blob := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))

	digest := reg.putImage("app", types.OCIManifestSchema1, blob)
	reg.assertManifestBody("app", digest.String(), blob)

	reg.clock.advance(61 * time.Second)
	reg.assertManifestStatus("app", digest.String(), http.StatusNotFound)
}

func TestRegistryReadingAManifestRefreshesIt(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute})
	blob := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	digest := reg.putImage("app", types.OCIManifestSchema1, blob)

	// Read it every 50 seconds and a one minute TTL never elapses.
	for range 5 {
		reg.clock.advance(50 * time.Second)
		if recorder := reg.do(http.MethodHead, "/v2/app/manifests/"+digest.String(), nil, nil); recorder.Code != http.StatusOK {
			t.Fatalf("HEAD of a manifest read 50s ago got %d, want 200", recorder.Code)
		}
	}
	reg.assertManifestBody("app", digest.String(), blob)

	// Stop reading it and it goes.
	reg.clock.advance(61 * time.Second)
	reg.assertManifestStatus("app", digest.String(), http.StatusNotFound)
}

func TestRegistryTagsAreImmortalByDefault(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute})
	image := newMultiPlatformImage(t)

	reg.putImage("app", types.OCIManifestSchema1, image.child)
	reg.putImage("app", types.OCIImageIndex, image.index)
	reg.putManifest("app", "v1", types.OCIImageIndex, image.index)

	reg.clock.advance(100 * time.Minute)
	reg.assertManifestBody("app", "v1", image.index)
	reg.assertManifestBody("app", image.indexDigest.String(), image.index)
	reg.assertManifestBody("app", image.childDigest.String(), image.child)
}

func TestRegistryDeleteByTagOnlyUntags(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute})
	blob := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	digest := reg.putManifest("app", "v1", types.OCIManifestSchema1, blob)

	if recorder := reg.do(http.MethodDelete, "/v2/app/manifests/v1", nil, nil); recorder.Code != http.StatusAccepted {
		t.Fatalf("DELETE of a tag got %d, want 202", recorder.Code)
	}
	reg.assertManifestStatus("app", "v1", http.StatusNotFound)
	reg.assertManifestBody("app", digest.String(), blob)
}

func TestRegistryDeleteByDigestRemovesTagsPointingAtIt(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute})
	blob := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	digest := reg.putManifest("app", "v1", types.OCIManifestSchema1, blob)
	reg.putManifest("app", "latest", types.OCIManifestSchema1, blob)

	if recorder := reg.do(http.MethodDelete, "/v2/app/manifests/"+digest.String(), nil, nil); recorder.Code != http.StatusAccepted {
		t.Fatalf("DELETE of a digest got %d, want 202", recorder.Code)
	}
	// Both tags resolved to the deleted manifest, so neither can resolve now.
	reg.assertManifestStatus("app", "v1", http.StatusNotFound)
	reg.assertManifestStatus("app", "latest", http.StatusNotFound)
	reg.assertManifestStatus("app", digest.String(), http.StatusNotFound)
}

func TestRegistryWithoutEvictionKeepsEverything(t *testing.T) {
	reg := newTestRegistryWithoutEviction(t)
	image := newMultiPlatformImage(t)

	reg.putImage("app", types.OCIManifestSchema1, image.child)
	reg.putImage("app", types.OCIImageIndex, image.index)
	digest := reg.putManifest("app", "v1", types.OCIImageIndex, image.index)

	reg.assertManifestBody("app", "v1", image.index)
	reg.assertManifestBody("app", image.childDigest.String(), image.child)

	// Untagging still leaves the manifest reachable by digest, which is how this
	// registry has always behaved without eviction.
	if recorder := reg.do(http.MethodDelete, "/v2/app/manifests/v1", nil, nil); recorder.Code != http.StatusAccepted {
		t.Fatalf("DELETE of a tag got %d, want 202", recorder.Code)
	}
	reg.assertManifestStatus("app", "v1", http.StatusNotFound)
	reg.assertManifestBody("app", digest.String(), image.index)
}

func TestRegistryTagsListLeavesOutUntaggedManifests(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Hour})
	image := newMultiPlatformImage(t)

	reg.putImage("app", types.OCIManifestSchema1, image.child)
	reg.putManifest("app", "v1", types.OCIImageIndex, image.index)
	reg.putManifest("app", "latest", types.OCIImageIndex, image.index)

	recorder := reg.do(http.MethodGet, "/v2/app/tags/list", nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET tags/list got %d, want 200", recorder.Code)
	}
	var listed listTags
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("parsing tags/list: %v", err)
	}
	if listed.Name != "app" || len(listed.Tags) != 2 || listed.Tags[0] != "latest" || listed.Tags[1] != "v1" {
		t.Fatalf("GET tags/list got %+v, want app with [latest v1]", listed)
	}

	recorder = reg.do(http.MethodGet, "/v2/_catalog", nil, nil)
	var listedRepos catalog
	if err := json.Unmarshal(recorder.Body.Bytes(), &listedRepos); err != nil {
		t.Fatalf("parsing _catalog: %v", err)
	}
	if len(listedRepos.Repos) != 1 || listedRepos.Repos[0] != "app" {
		t.Fatalf("GET _catalog got %+v, want [app]", listedRepos)
	}
}

func TestRegistryUnknownNameAndUnknownManifest(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Hour})

	assertErrorCode(t, reg.do(http.MethodGet, "/v2/nope/manifests/v1", nil, nil), http.StatusNotFound, "NAME_UNKNOWN")
	assertErrorCode(t, reg.do(http.MethodGet, "/v2/nope/tags/list", nil, nil), http.StatusNotFound, "NAME_UNKNOWN")

	reg.putManifest("app", "v1", types.OCIManifestSchema1, imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config")))
	assertErrorCode(t, reg.do(http.MethodGet, "/v2/app/manifests/missing", nil, nil), http.StatusNotFound, "MANIFEST_UNKNOWN")
	missing := descriptorFor(types.OCIManifestSchema1, "never pushed").Digest
	assertErrorCode(t, reg.do(http.MethodGet, "/v2/app/manifests/"+missing.String(), nil, nil), http.StatusNotFound, "MANIFEST_UNKNOWN")
}

func TestRegistryRejectsAnIndexWithMissingChildren(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Hour})
	image := newMultiPlatformImage(t)

	// The child was never pushed.
	assertErrorCode(t, reg.do(
		http.MethodPut,
		"/v2/app/manifests/v1",
		image.index,
		http.Header{"Content-Type": []string{string(types.OCIImageIndex)}},
	), http.StatusNotFound, "MANIFEST_UNKNOWN")

	// An index whose Content-Type says otherwise is still an index, and its
	// children are still checked.
	assertErrorCode(t, reg.do(
		http.MethodPut,
		"/v2/app/manifests/v1",
		image.index,
		http.Header{"Content-Type": []string{string(types.OCIManifestSchema1)}},
	), http.StatusNotFound, "MANIFEST_UNKNOWN")
}

func TestRegistryRejectsAPushUnderTheWrongDigest(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Hour})
	blob := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	wrong := descriptorFor(types.OCIManifestSchema1, "some other manifest").Digest

	// Storing it under a digest its bytes do not hash to would leave a
	// reference nobody can pull by any other name.
	assertErrorCode(t, reg.do(
		http.MethodPut,
		"/v2/app/manifests/"+wrong.String(),
		blob,
		http.Header{"Content-Type": []string{string(types.OCIManifestSchema1)}},
	), http.StatusBadRequest, "DIGEST_INVALID")
	reg.assertManifestStatus("app", wrong.String(), http.StatusNotFound)
}

func TestRegistryReferrersSurviveTheirSubject(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute})
	config := descriptorFor(types.OCIConfigJSON, "config")
	image := imageManifest(t, "image", config)
	imageDigest := digestOf(t, image)
	signature := referrerManifest(t, "signature", imageDigest, descriptorFor(types.OCIConfigJSON, "signature-config"))
	signatureDigest := digestOf(t, signature)

	reg.putManifest("app", "v1", types.OCIManifestSchema1, image)
	reg.putImage("app", types.OCIManifestSchema1, signature)

	// The signature is nobody's child, so only the subject edge keeps it. Its
	// subject is tagged, so it outlives its own TTL many times over.
	reg.clock.advance(100 * time.Minute)
	reg.assertManifestBody("app", signatureDigest.String(), signature)

	recorder := reg.do(http.MethodGet, "/v2/app/referrers/"+imageDigest.String(), nil, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET referrers got %d, want 200", recorder.Code)
	}
	var referrers v1.IndexManifest
	if err := json.Unmarshal(recorder.Body.Bytes(), &referrers); err != nil {
		t.Fatalf("parsing referrers: %v", err)
	}
	if len(referrers.Manifests) != 1 || referrers.Manifests[0].Digest != signatureDigest {
		t.Fatalf("GET referrers listed %+v, want just the signature", referrers.Manifests)
	}
}

func TestRegistryCollectsBlobContentsWhenPruningIsAllowed(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute})
	content := []byte("an unreferenced blob")
	digest := digestOf(t, content)

	if recorder := reg.do(http.MethodPost, "/v2/app/blobs/uploads/?digest="+digest.String(), content, nil); recorder.Code != http.StatusCreated {
		t.Fatalf("POST of a blob got %d and body %q, want 201", recorder.Code, recorder.Body)
	}
	if recorder := reg.do(http.MethodHead, "/v2/app/blobs/"+digest.String(), nil, nil); recorder.Code != http.StatusOK {
		t.Fatalf("HEAD of a pushed blob got %d, want 200", recorder.Code)
	}

	// Nothing ever named it, so once it goes cold the bytes go too. Blob
	// requests do not sweep -- they are the hot path -- so sweep explicitly.
	reg.clock.advance(61 * time.Second)
	if stats := reg.collector.Collect(); stats.Blobs != 1 {
		t.Fatalf("collecting took %d blobs, want 1", stats.Blobs)
	}
	if recorder := reg.do(http.MethodHead, "/v2/app/blobs/"+digest.String(), nil, nil); recorder.Code != http.StatusNotFound {
		t.Fatalf("HEAD of a collected blob got %d, want 404", recorder.Code)
	}
}

func TestRegistryKeepsBlobContentsWhenPruningIsOff(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute}, WithBlobPruning(false))
	content := []byte("an unreferenced blob")
	digest := digestOf(t, content)

	if recorder := reg.do(http.MethodPost, "/v2/app/blobs/uploads/?digest="+digest.String(), content, nil); recorder.Code != http.StatusCreated {
		t.Fatalf("POST of a blob got %d and body %q, want 201", recorder.Code, recorder.Body)
	}

	reg.clock.advance(61 * time.Second)
	if stats := reg.collector.Collect(); stats.Blobs != 1 {
		t.Fatalf("collecting took %d blobs, want 1", stats.Blobs)
	}
	if recorder := reg.do(http.MethodHead, "/v2/app/blobs/"+digest.String(), nil, nil); recorder.Code != http.StatusOK {
		t.Fatalf("HEAD of a blob in a store we may not prune got %d, want 200", recorder.Code)
	}
}

func TestRegistryKeepsBlobsAManifestNames(t *testing.T) {
	reg := newTestRegistry(t, CollectorConfig{TTL: time.Minute})
	layer := []byte("a layer a manifest names")
	layerDigest := digestOf(t, layer)
	config := []byte("a config a manifest names")
	configDigest := digestOf(t, config)

	for _, content := range [][]byte{layer, config} {
		digest := digestOf(t, content)
		if recorder := reg.do(http.MethodPost, "/v2/app/blobs/uploads/?digest="+digest.String(), content, nil); recorder.Code != http.StatusCreated {
			t.Fatalf("POST of a blob got %d and body %q, want 201", recorder.Code, recorder.Body)
		}
	}
	blob := imageManifest(t,
		"image",
		v1.Descriptor{MediaType: types.OCIConfigJSON, Digest: configDigest, Size: int64(len(config))},
		v1.Descriptor{MediaType: types.OCILayer, Digest: layerDigest, Size: int64(len(layer))},
	)
	reg.putManifest("app", "v1", types.OCIManifestSchema1, blob)

	reg.clock.advance(100 * time.Minute)
	if stats := reg.collector.Collect(); stats != (CollectStats{}) {
		t.Fatalf("collecting a tagged image removed %+v, want nothing", stats)
	}
	for _, digest := range []v1.Hash{layerDigest, configDigest} {
		if recorder := reg.do(http.MethodHead, "/v2/app/blobs/"+digest.String(), nil, nil); recorder.Code != http.StatusOK {
			t.Fatalf("HEAD of a blob a tagged manifest names got %d, want 200", recorder.Code)
		}
	}
}

func TestNewAdoptsTheCollectorsStore(t *testing.T) {
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Minute})
	handler := New(WithCollector(collector), Logger(log.New(io.Discard, "", 0)))

	blob := imageManifest(t, "image", descriptorFor(types.OCIConfigJSON, "config"))
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/app/manifests/v1", bytes.NewReader(blob))
	req.Header.Set("Content-Type", string(types.OCIManifestSchema1))
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("PUT got %d, want 201", recorder.Code)
	}
	if _, ok := store.ResolveTag("app", "v1"); !ok {
		t.Fatal("the registry did not write to the collector's store")
	}
}

func TestNewRejectsAStoreTheCollectorDoesNotUse(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a collector built over a different store")
		}
	}()
	collector := NewCollector(NewMemStore(), CollectorConfig{TTL: time.Minute})
	New(WithStore(NewMemStore()), WithCollector(collector))
}

// TestRegistryKeepsTaggedManifestsWhileCollectingConcurrently sweeps as hard as
// it can while clients push and pull, using a real clock and a TTL of a
// nanosecond so a sweep runs on practically every request. Anything reachable
// from a tag has to survive that; run under -race it also covers the lock order
// between the collector and the store.
func TestRegistryKeepsTaggedManifestsWhileCollectingConcurrently(t *testing.T) {
	store := NewMemStore()
	collector := NewCollector(store, CollectorConfig{TTL: time.Nanosecond, Interval: time.Nanosecond})
	handler := New(
		WithStore(store),
		WithCollector(collector),
		Logger(log.New(io.Discard, "", 0)),
	)

	const clients = 8
	const pushes = 20
	sweeping := make(chan struct{})
	var sweeper sync.WaitGroup
	sweeper.Add(1)
	go func() {
		defer sweeper.Done()
		for {
			select {
			case <-sweeping:
				return
			default:
				collector.Collect()
			}
		}
	}()

	var clientsDone sync.WaitGroup
	for client := range clients {
		clientsDone.Add(1)
		go func() {
			defer clientsDone.Done()
			for push := range pushes {
				name := fmt.Sprintf("client-%d-push-%d", client, push)
				blob := imageManifest(t, name, descriptorFor(types.OCIConfigJSON, name))

				recorder := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPut, "/v2/app/manifests/"+name, bytes.NewReader(blob))
				req.Header.Set("Content-Type", string(types.OCIManifestSchema1))
				handler.ServeHTTP(recorder, req)
				if recorder.Code != http.StatusCreated {
					t.Errorf("PUT %s got %d, want 201", name, recorder.Code)
					return
				}

				// A tag is a root, so the manifest behind it must resolve no
				// matter how many sweeps happened in between.
				recorder = httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v2/app/manifests/"+name, nil))
				if recorder.Code != http.StatusOK {
					t.Errorf("GET %s got %d, want 200: a tagged manifest was collected", name, recorder.Code)
					return
				}
			}
		}()
	}
	clientsDone.Wait()
	close(sweeping)
	sweeper.Wait()

	for client := range clients {
		for push := range pushes {
			name := fmt.Sprintf("client-%d-push-%d", client, push)
			if _, ok := store.ResolveTag("app", name); !ok {
				t.Fatalf("tag %s is gone, want it kept", name)
			}
		}
	}
}

func assertErrorCode(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	if recorder.Code != wantStatus {
		t.Fatalf("got status %d and body %q, want %d", recorder.Code, recorder.Body, wantStatus)
	}
	var body struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("parsing error body %q: %v", recorder.Body, err)
	}
	if len(body.Errors) != 1 || body.Errors[0].Code != wantCode {
		t.Fatalf("got error body %q, want code %s", recorder.Body, wantCode)
	}
}
