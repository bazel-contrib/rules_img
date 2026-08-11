package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/ocilayout"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// naiveRegistry is an in-process OCI registry that models the case the
// deduplicated push strategy exists for: blob storage is scoped to a repository
// name, so a blob uploaded to one repository is invisible from another, while
// cross-repository mounts do work.
//
// go-containerregistry's own test registry cannot stand in for it: its blob
// handler ignores the repository argument, so every blob HEAD succeeds across
// repositories and the mount path is never reached. It also does not implement
// mounts at all.
type naiveRegistry struct {
	mu sync.Mutex
	// blobs and manifests are both keyed repository -> identifier. Manifests are
	// stored under their digest and under every tag pointing at them.
	blobs     map[string]map[string][]byte
	manifests map[string]map[string]storedManifest
	uploads   map[string][]byte
	nextID    int

	// mountable turns cross-repository mounts on. Turning it off models a registry
	// that does not support them, which must make a mount-only layer fail rather
	// than quietly re-upload.
	mountable bool

	// blobPuts records every finalized blob upload as "<repository>@<digest>", and
	// mounts every satisfied mount as "<repository>@<digest>". A blob that appears
	// once in blobPuts and several times in mounts is the whole point of the
	// strategy.
	blobPuts     []string
	mounts       []string
	manifestPuts []string
	// mountOrigins records the "origin" query parameter of every satisfied mount, in
	// step with mounts. go-containerregistry only sends it -- and only asks the token
	// service for read access to the source repository -- when the cross-mount source
	// names a registry, so an empty entry here means the deploy recorded a source that
	// a scope-enforcing registry would refuse to mount from.
	mountOrigins []string
	// crossRegistryMounts records every mount attempt whose "origin" named another
	// registry. It must stay empty: see startUpload.
	crossRegistryMounts []string
}

type storedManifest struct {
	mediaType string
	digest    string
	body      []byte
}

func newNaiveRegistry() *naiveRegistry {
	return &naiveRegistry{
		blobs:     map[string]map[string][]byte{},
		manifests: map[string]map[string]storedManifest{},
		uploads:   map[string][]byte{},
		mountable: true,
	}
}

// transport serves the registry in-process, so the test needs no listening socket.
func (r *naiveRegistry) transport() http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		// A client-built request may carry a nil Body, which a handler reached
		// through a real server never sees.
		served := req.Clone(req.Context())
		if served.Body == nil {
			served.Body = http.NoBody
		}
		recorder := httptest.NewRecorder()
		r.ServeHTTP(recorder, served)
		if req.Body != nil {
			req.Body.Close()
		}
		resp := recorder.Result()
		resp.Request = req
		return resp, nil
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// registryFleet serves several naiveRegistries through one transport, routed by host,
// so a test can model a deploy that pushes to more than one registry. Each registry
// keeps its own blob storage, which is what makes a mount between them impossible --
// exactly like the registries that do not implement cross-registry mounts.
type registryFleet map[string]*naiveRegistry

func (f registryFleet) transport() http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		registry, found := f[req.URL.Host]
		if !found {
			return nil, fmt.Errorf("no registry serving %s", req.URL.Host)
		}
		return registry.transport().RoundTrip(req)
	})
}

func (r *naiveRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/v2" || req.URL.Path == "/v2/" {
		w.WriteHeader(http.StatusOK)
		return
	}
	// /v2/<repository...>/{blobs,manifests}/<target>
	elem := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if len(elem) < 4 || elem[0] != "v2" {
		http.Error(w, "unexpected path", http.StatusBadRequest)
		return
	}
	target := elem[len(elem)-1]
	kind := elem[len(elem)-2]
	repository := path.Join(elem[1 : len(elem)-2]...)
	if kind == "uploads" {
		// /v2/<repository...>/blobs/uploads/<id>
		kind = "uploads"
		repository = path.Join(elem[1 : len(elem)-3]...)
	}

	switch {
	case kind == "blobs" && target == "uploads":
		r.startUpload(w, req, repository)
	case kind == "uploads":
		r.continueUpload(w, req, repository, target)
	case kind == "blobs":
		r.serveBlob(w, req, repository, target)
	case kind == "manifests":
		r.serveManifest(w, req, repository, target)
	default:
		http.Error(w, "unexpected path", http.StatusBadRequest)
	}
}

// startUpload handles POST /v2/<repository>/blobs/uploads/, satisfying a
// cross-repository mount when the source repository holds the blob and mounts are
// enabled, and otherwise opening an upload session.
//
// A mount whose "origin" names another registry is refused the way a registry
// without cross-registry mount support refuses it -- by opening an upload session
// instead. Few registries implement those, so the strategy must never depend on one:
// this is what holds it to mounts between two repositories of the same registry.
func (r *naiveRegistry) startUpload(w http.ResponseWriter, req *http.Request, repository string) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	query := req.URL.Query()
	mount, from, origin := query.Get("mount"), query.Get("from"), query.Get("origin")
	sameRegistry := origin == "" || origin == req.URL.Host
	if r.mountable && sameRegistry && mount != "" && from != "" {
		if content, found := r.blobs[from][mount]; found {
			r.putBlobLocked(repository, mount, content)
			r.mounts = append(r.mounts, repository+"@"+mount)
			r.mountOrigins = append(r.mountOrigins, origin)
			w.Header().Set("Location", "/v2/"+repository+"/blobs/"+mount)
			w.Header().Set("Docker-Content-Digest", mount)
			w.WriteHeader(http.StatusCreated)
			return
		}
	}
	if origin != "" && !sameRegistry {
		r.crossRegistryMounts = append(r.crossRegistryMounts, repository+"@"+mount+" from "+origin+"/"+from)
	}

	r.nextID++
	id := fmt.Sprintf("upload-%d", r.nextID)
	r.uploads[id] = nil
	w.Header().Set("Location", "/v2/"+repository+"/blobs/uploads/"+id)
	w.Header().Set("Range", "0-0")
	w.WriteHeader(http.StatusAccepted)
}

// continueUpload handles the PATCH (stream) and PUT (finalize) halves of an
// upload session.
func (r *naiveRegistry) continueUpload(w http.ResponseWriter, req *http.Request, repository, id string) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	switch req.Method {
	case http.MethodPatch:
		r.uploads[id] = append(r.uploads[id], body...)
		w.Header().Set("Location", "/v2/"+repository+"/blobs/uploads/"+id)
		w.Header().Set("Range", fmt.Sprintf("0-%d", len(r.uploads[id])-1))
		w.WriteHeader(http.StatusAccepted)
	case http.MethodPut:
		digest := req.URL.Query().Get("digest")
		if digest == "" {
			http.Error(w, "digest not specified", http.StatusBadRequest)
			return
		}
		content := append(r.uploads[id], body...)
		delete(r.uploads, id)
		if got := sha256Hash(content).String(); got != digest {
			http.Error(w, "digest mismatch: got "+got, http.StatusBadRequest)
			return
		}
		r.putBlobLocked(repository, digest, content)
		r.blobPuts = append(r.blobPuts, repository+"@"+digest)
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *naiveRegistry) serveBlob(w http.ResponseWriter, req *http.Request, repository, digest string) {
	r.mu.Lock()
	content, found := r.blobs[repository][digest]
	r.mu.Unlock()
	if !found {
		http.Error(w, "blob unknown", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(content)))
	w.Header().Set("Docker-Content-Digest", digest)
	if req.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(content)
}

func (r *naiveRegistry) serveManifest(w http.ResponseWriter, req *http.Request, repository, reference string) {
	if req.Method == http.MethodPut {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		digest := sha256Hash(body).String()
		stored := storedManifest{mediaType: req.Header.Get("Content-Type"), digest: digest, body: body}
		r.mu.Lock()
		if r.manifests[repository] == nil {
			r.manifests[repository] = map[string]storedManifest{}
		}
		r.manifests[repository][digest] = stored
		if reference != digest {
			r.manifests[repository][reference] = stored
		}
		r.manifestPuts = append(r.manifestPuts, repository+":"+reference)
		r.mu.Unlock()
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
		return
	}

	r.mu.Lock()
	stored, found := r.manifests[repository][reference]
	r.mu.Unlock()
	if !found {
		http.Error(w, "manifest unknown", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", stored.mediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(stored.body)))
	w.Header().Set("Docker-Content-Digest", stored.digest)
	if req.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write(stored.body)
}

func (r *naiveRegistry) putBlobLocked(repository, digest string, content []byte) {
	if r.blobs[repository] == nil {
		r.blobs[repository] = map[string][]byte{}
	}
	r.blobs[repository][digest] = content
}

// countBlobPuts returns how many times the given blob's bytes were uploaded, to
// any repository.
func (r *naiveRegistry) countBlobPuts(digest string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, put := range r.blobPuts {
		if strings.HasSuffix(put, "@"+digest) {
			count++
		}
	}
	return count
}

// putBlob stores a blob in a repository, taking the lock the way a request handler
// does.
func (r *naiveRegistry) putBlob(repository, digest string, content []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.putBlobLocked(repository, digest, content)
}

// hasBlob reports whether the repository holds the blob.
func (r *naiveRegistry) hasBlob(repository, digest string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, found := r.blobs[repository][digest]
	return found
}

// storedManifestFor returns the manifest a repository holds under an identifier (a
// digest or a tag), and whether it holds one at all.
func (r *naiveRegistry) storedManifestFor(repository, identifier string) (storedManifest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, found := r.manifests[repository][identifier]
	return stored, found
}

func (r *naiveRegistry) snapshot() (blobPuts, mounts, manifestPuts []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	blobPuts = append(blobPuts, r.blobPuts...)
	mounts = append(mounts, r.mounts...)
	manifestPuts = append(manifestPuts, r.manifestPuts...)
	sort.Strings(blobPuts)
	sort.Strings(mounts)
	sort.Strings(manifestPuts)
	return blobPuts, mounts, manifestPuts
}

// sharedLayerImages describes the images written by buildSharedLayerLayout.
type sharedLayerImages struct {
	registry     string
	repositories []string
	// shared holds the layer digests every image references.
	shared []string
	// unique holds each image's own layer digest, in repository order.
	unique []string
	// configs and manifests hold each image's config and manifest digest.
	configs   []string
	manifests []string
	// blobs holds every blob of every image -- manifests, configs and layers --
	// keyed by hex digest, so a test can serve them from somewhere other than the
	// OCI layouts (see TestDeduplicatedPushReadsEachBlobFromTheCASOnce).
	blobs map[string][]byte
}

// buildSharedLayerLayouts writes one OCI layout per service image -- as one
// image_push target's runfiles would -- with all of them sharing the same base
// layers and differing only in their last layer. It returns the layout
// directories plus a deploy manifest pushing each image to its own repository:
// the customer shape the deduplicated push exists for.
//
// One layout per image because the OCI layout format writes a single-manifest
// index.json (see ocilayout.IndexClean).
func buildSharedLayerLayouts(t *testing.T, registry string, services, sharedLayers int) ([]string, api.DeployManifest, sharedLayerImages) {
	t.Helper()
	images := sharedLayerImages{registry: registry, blobs: map[string][]byte{}}

	sharedContent := make([][]byte, sharedLayers)
	for i := range sharedContent {
		sharedContent[i] = []byte(fmt.Sprintf("shared-layer-%d", i))
		images.shared = append(images.shared, sha256Hash(sharedContent[i]).String())
	}

	var layoutDirs []string
	var operations []json.RawMessage
	for service := range services {
		repository := fmt.Sprintf("team/service-%d", service)
		config := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","service":%d}`, service))
		unique := []byte(fmt.Sprintf("service-%d-layer", service))

		src := ocilayout.NewMemBlobSource()
		src.Add(sha256Hash(config).Hex, config).Add(sha256Hash(unique).Hex, unique)
		images.blobs[sha256Hash(config).Hex] = config
		images.blobs[sha256Hash(unique).Hex] = unique
		descriptors := make([]registryv1.Descriptor, 0, sharedLayers+1)
		layerBlobs := make([]api.LayerBlob, 0, sharedLayers+1)
		for _, content := range append(append([][]byte{}, sharedContent...), unique) {
			src.Add(sha256Hash(content).Hex, content)
			images.blobs[sha256Hash(content).Hex] = content
			descriptor := registryv1.Descriptor{MediaType: types.OCILayer, Digest: sha256Hash(content), Size: int64(len(content))}
			descriptors = append(descriptors, descriptor)
			layerBlobs = append(layerBlobs, api.LayerBlob{Descriptor: api.Descriptor{
				MediaType: string(types.OCILayer), Digest: descriptor.Digest.String(), Size: descriptor.Size,
			}})
		}

		manifest := &registryv1.Manifest{
			SchemaVersion: 2,
			MediaType:     types.OCIManifestSchema1,
			Config:        registryv1.Descriptor{MediaType: types.OCIConfigJSON, Digest: sha256Hash(config), Size: int64(len(config))},
			Layers:        descriptors,
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			t.Fatalf("marshalling manifest: %v", err)
		}
		layoutDir := t.TempDir()
		layout := ocilayout.New(ocilayout.OCILayout()).AddManifest(ocilayout.ManifestInputFromVFS(src, manifest, raw, nil))
		if err := layout.WriteDir(context.Background(), layoutDir); err != nil {
			t.Fatalf("writing layout: %v", err)
		}
		layoutDirs = append(layoutDirs, layoutDir)

		operation, err := json.Marshal(api.PushDeployOperation{
			BaseCommandOperation: api.BaseCommandOperation{
				Command:          "push",
				RootKind:         "manifest",
				DeduplicatedPush: true,
				Root:             api.Descriptor{MediaType: string(types.OCIManifestSchema1), Digest: sha256Hash(raw).String(), Size: int64(len(raw))},
				Manifests: []api.ManifestDeployInfo{{
					Descriptor: api.Descriptor{MediaType: string(types.OCIManifestSchema1), Digest: sha256Hash(raw).String(), Size: int64(len(raw))},
					Config:     api.Descriptor{MediaType: string(types.OCIConfigJSON), Digest: manifest.Config.Digest.String(), Size: manifest.Config.Size},
					LayerBlobs: layerBlobs,
				}},
			},
			PushTarget: api.PushTarget{Registry: registry, Repository: repository, Tags: []string{"latest"}},
		})
		if err != nil {
			t.Fatalf("marshalling push operation: %v", err)
		}
		operations = append(operations, operation)

		images.repositories = append(images.repositories, repository)
		images.unique = append(images.unique, sha256Hash(unique).String())
		images.configs = append(images.configs, sha256Hash(config).String())
		images.manifests = append(images.manifests, sha256Hash(raw).String())
		images.blobs[sha256Hash(raw).Hex] = raw
	}

	return layoutDirs, api.DeployManifest{
		Operations: operations,
		Settings:   api.DeploySettings{PushStrategy: "eager"},
	}, images
}

// runDedupDeploy performs the deduplicated push the way DeployWithExtras does:
// prepare the phases, then hand the resulting mount-only view to the ordinary
// manifest push.
//
// It also holds every one of these tests to the no-cross-registry invariant: whatever
// the plan decided, the registry must never have been asked to fetch a blob from
// another registry.
func runDedupDeploy(t *testing.T, registry *naiveRegistry, layoutDirs []string, dm api.DeployManifest) error {
	t.Helper()
	defer registry.assertNoCrossRegistryMounts(t)
	return runDedupDeployVia(t, registry.transport(), layoutDirs, dm)
}

// assertNoCrossRegistryMounts fails the test if the registry was ever asked to fetch
// a blob from another registry.
func (r *naiveRegistry) assertNoCrossRegistryMounts(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.crossRegistryMounts) > 0 {
		t.Errorf("registry saw cross-registry mount attempts, want none: %v", r.crossRegistryMounts)
	}
}

// runDedupDeployVia is runDedupDeploy against an arbitrary transport, so a test can
// serve more than one registry (see registryFleet).
func runDedupDeployVia(t *testing.T, transport http.RoundTripper, layoutDirs []string, dm api.DeployManifest) error {
	t.Helper()
	vfsBuilder := deployvfs.NewBuilder(dm)
	for _, layoutDir := range layoutDirs {
		vfsBuilder = vfsBuilder.WithOCILayout(layoutDir)
	}
	vfs, err := vfsBuilder.Build()
	if err != nil {
		t.Fatalf("building VFS: %v", err)
	}
	pushOps, err := dm.PushOperations()
	if err != nil {
		t.Fatalf("reading push operations: %v", err)
	}

	ctx := context.Background()
	selector := dedupSelector{}
	views, err := prepareDedupPush(ctx, vfs, pushOps, nil, dedupOptions{
		selector:       selector,
		blobRepository: dm.Settings.BlobRepository,
		jobs:           4,
		forbidUpload:   dm.Settings.ForbidLayerPush,
		pushTransport:  transport,
	})
	if err != nil {
		return err
	}
	_, err = push.NewBuilder(vfs).
		WithVFSForOperation(func(op api.IndexedPushDeployOperation) push.VFS {
			return views.For(op.Registry, op.BaseCommandOperation)
		}).
		WithJobs(4).
		WithRemoteOptions(registryopts.Default().WithTransport(transport).Remote()...).
		Build().
		PushAll(ctx, pushOps, dm.Settings.PushStrategy)
	return err
}

// TestDeduplicatedPushUploadsSharedLayersOnce is the strategy's reason to exist:
// against a registry with per-repository blob storage, three services sharing two
// layers must upload each shared layer's bytes exactly once -- to the first of
// their repositories alphabetically -- and cross-mount it into the other two.
func TestDeduplicatedPushUploadsSharedLayersOnce(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 3, 2)
	home := images.repositories[0] // team/service-0, first alphabetically

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}

	// Each shared layer's bytes crossed the wire exactly once, into the home
	// repository. Without the strategy each would be uploaded once per destination.
	for _, digest := range images.shared {
		if got := reg.countBlobPuts(digest); got != 1 {
			t.Errorf("shared layer %s uploaded %d times, want exactly 1", digest, got)
		}
		if !reg.hasBlob(home, digest) {
			t.Errorf("shared layer %s is not in the home repository %s", digest, home)
		}
	}
	// A layer only one service needs is not deduplicated at all: it goes straight to
	// that service's repository as part of the ordinary manifest push.
	for i, digest := range images.unique {
		if got := reg.countBlobPuts(digest); got != 1 {
			t.Errorf("layer %s uploaded %d times, want exactly 1", digest, got)
		}
		if !reg.hasBlob(images.repositories[i], digest) {
			t.Errorf("layer %s never reached %s", digest, images.repositories[i])
		}
	}

	blobPuts, mounts, _ := reg.snapshot()
	// Two shared layers cross-mounted into the two repositories that are not the
	// home. Nothing else is mounted.
	if len(mounts) != 4 {
		t.Errorf("registry satisfied %d mounts, want 4 (2 shared layers x 2 non-home repositories): %v", len(mounts), mounts)
	}
	// Every one of them named the registry the source repository is in. Without that,
	// go-containerregistry never asks the token service for read access to the source,
	// and a registry that enforces per-repository scopes cannot authorize the mount --
	// which for a mount-only layer means a failed push, not a slower one.
	reg.mu.Lock()
	origins := append([]string(nil), reg.mountOrigins...)
	reg.mu.Unlock()
	for i, origin := range origins {
		if origin != images.registry {
			t.Errorf("mount %d carried origin %q, want the destination registry %q", i, origin, images.registry)
		}
	}
	for _, repository := range images.repositories[1:] {
		for _, digest := range images.shared {
			if !reg.hasBlob(repository, digest) {
				t.Errorf("shared layer %s never reached %s", digest, repository)
			}
		}
	}

	// Config blobs are the exception: go-containerregistry uploads them from an
	// in-memory layer that cannot be mounted, so each destination repository gets
	// its own copy. They are per-image and tiny, so there is nothing to deduplicate.
	for i, repository := range images.repositories {
		if !reg.hasBlob(repository, images.configs[i]) {
			t.Errorf("config %s never reached %s", images.configs[i], repository)
		}
	}

	// 2 shared layers + 1 unique layer and 1 config per service.
	want := len(images.shared) + 2*len(images.repositories)
	if len(blobPuts) != want {
		t.Errorf("registry accepted %d blob uploads, want %d (2 shared once + 1 unique + 1 config per service): %v",
			len(blobPuts), want, blobPuts)
	}

	// Every service is published under its digest and its tag.
	for i, repository := range images.repositories {
		if _, found := reg.storedManifestFor(repository, images.manifests[i]); !found {
			t.Errorf("manifest %s never reached %s", images.manifests[i], repository)
		}
		if stored, found := reg.storedManifestFor(repository, "latest"); !found || stored.digest != images.manifests[i] {
			t.Errorf("tag latest in %s = %+v, want it to point at %s", repository, stored.digest, images.manifests[i])
		}
	}
}

// TestDeduplicatedPushDeduplicatesEachRegistrySeparately is the multi-registry case:
// the same images pushed to two registries. A blob store and a cross-mount both live
// in one registry, so each registry gets its own upload of the shared layer and its
// own mounts -- and never a mount naming the other registry, which is what a registry
// without cross-registry mount support would refuse.
func TestDeduplicatedPushDeduplicatesEachRegistrySeparately(t *testing.T) {
	mirror := "b.example.com"
	primary := newNaiveRegistry()
	secondary := newNaiveRegistry()
	fleet := registryFleet{"a.example.com": primary, mirror: secondary}

	layoutDirs, dm, images := buildSharedLayerLayouts(t, "a.example.com", 3, 2)
	dm = withMirroredRegistry(t, dm, mirror)
	home := images.repositories[0] // team/service-0, first alphabetically

	if err := runDedupDeployVia(t, fleet.transport(), layoutDirs, dm); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}

	for name, reg := range fleet {
		reg.assertNoCrossRegistryMounts(t)
		for _, digest := range images.shared {
			if got := reg.countBlobPuts(digest); got != 1 {
				t.Errorf("%s: shared layer %s uploaded %d times, want exactly 1", name, digest, got)
			}
			if !reg.hasBlob(home, digest) {
				t.Errorf("%s: shared layer %s is not in the home repository %s", name, digest, home)
			}
			for _, repository := range images.repositories[1:] {
				if !reg.hasBlob(repository, digest) {
					t.Errorf("%s: shared layer %s never reached %s", name, digest, repository)
				}
			}
		}
		_, mounts, _ := reg.snapshot()
		if len(mounts) != 4 {
			t.Errorf("%s: satisfied %d mounts, want 4 (2 shared layers x 2 non-home repositories): %v", name, len(mounts), mounts)
		}
		reg.mu.Lock()
		origins := append([]string(nil), reg.mountOrigins...)
		reg.mu.Unlock()
		for i, origin := range origins {
			if origin != name {
				t.Errorf("%s: mount %d carried origin %q, want its own registry", name, i, origin)
			}
		}
	}
}

// TestDeduplicatedPushLeavesSingleImageDeploysAlone verifies the strategy is a
// no-op when there is nothing to deduplicate: one image means one destination
// repository, so no blob is uploaded anywhere but its own repository and nothing is
// cross-mounted.
func TestDeduplicatedPushLeavesSingleImageDeploysAlone(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 1, 2)

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}

	_, mounts, _ := reg.snapshot()
	if len(mounts) != 0 {
		t.Errorf("registry satisfied %d mounts, want none for a single image: %v", len(mounts), mounts)
	}
	for _, digest := range append(append([]string{}, images.shared...), images.unique...) {
		if got := reg.countBlobPuts(digest); got != 1 {
			t.Errorf("layer %s uploaded %d times, want exactly 1", digest, got)
		}
		if !reg.hasBlob(images.repositories[0], digest) {
			t.Errorf("layer %s never reached %s", digest, images.repositories[0])
		}
	}
}

// TestNaiveRegistryUploadsSharedLayersPerRepository is the negative control for
// the test above: it pushes the same images the ordinary way, so the assertion
// that the strategy uploads each shared layer once means something. Against a
// registry with per-repository blob storage, the plain push uploads every shared
// layer once per destination repository.
func TestNaiveRegistryUploadsSharedLayersPerRepository(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 3, 2)

	vfsBuilder := deployvfs.NewBuilder(dm)
	for _, layoutDir := range layoutDirs {
		vfsBuilder = vfsBuilder.WithOCILayout(layoutDir)
	}
	vfs, err := vfsBuilder.Build()
	if err != nil {
		t.Fatalf("building VFS: %v", err)
	}
	pushOps, err := dm.PushOperations()
	if err != nil {
		t.Fatalf("reading push operations: %v", err)
	}
	if _, err := push.NewBuilder(vfs).
		WithJobs(4).
		WithRemoteOptions(registryopts.Default().WithTransport(reg.transport()).Remote()...).
		Build().
		PushAll(context.Background(), pushOps, dm.Settings.PushStrategy); err != nil {
		t.Fatalf("plain push: %v", err)
	}

	for _, digest := range images.shared {
		if got := reg.countBlobPuts(digest); got != len(images.repositories) {
			t.Errorf("plain push uploaded shared layer %s %d times, want once per repository (%d)",
				digest, got, len(images.repositories))
		}
	}
}

// TestDeduplicatedPushMixesOptedInAndOptedOutOperations is the mixed deploy: two
// services push to a registry that cross-mounts blobs, one to a destination that is
// not assumed to. The shared layer is uploaded once for the two that opted in and
// cross-mounted between them, while the third uploads its own copy and is never
// handed a mount.
func TestDeduplicatedPushMixesOptedInAndOptedOutOperations(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 3, 1)
	optedOut := images.repositories[2] // team/service-2
	dm = withoutDedup(t, dm, 2)

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}

	shared := images.shared[0]
	home := images.repositories[0] // team/service-0, first of the opted-in repositories
	// Once for the two operations that opted in, plus the opted-out one's own copy.
	if got := reg.countBlobPuts(shared); got != 2 {
		t.Errorf("the shared layer was uploaded %d times, want 2 (once for the opted-in pair, once for the opted-out operation)", got)
	}
	for _, repository := range []string{home, optedOut} {
		if !reg.hasBlob(repository, shared) {
			t.Errorf("the shared layer never reached %s by upload", repository)
		}
	}

	_, mounts, _ := reg.snapshot()
	// Exactly one cross-mount: into the opted-in repository that is not the home.
	if len(mounts) != 1 {
		t.Fatalf("registry satisfied %d mounts, want 1: %v", len(mounts), mounts)
	}
	if want := images.repositories[1] + "@" + shared; mounts[0] != want {
		t.Errorf("mount = %q, want %q", mounts[0], want)
	}
	// The operation that opted out was never asked to mount anything.
	for _, mount := range mounts {
		if strings.HasPrefix(mount, optedOut+"@") {
			t.Errorf("%s received a cross-mount, but it did not opt in", optedOut)
		}
	}
	// Every service is still published.
	for i, repository := range images.repositories {
		if _, found := reg.storedManifestFor(repository, images.manifests[i]); !found {
			t.Errorf("manifest %s never reached %s", images.manifests[i], repository)
		}
	}
}

// TestDeduplicatedPushSecondRunIsIdempotent verifies the existence check earns its
// requests: re-deploying unchanged images uploads no blobs and writes no
// manifests.
func TestDeduplicatedPushSecondRunIsIdempotent(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, _ := buildSharedLayerLayouts(t, "reg.example.com", 3, 2)

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("first deduplicated push: %v", err)
	}
	firstBlobPuts, firstMounts, firstManifestPuts := reg.snapshot()

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("second deduplicated push: %v", err)
	}
	blobPuts, mounts, manifestPuts := reg.snapshot()

	if len(blobPuts) != len(firstBlobPuts) {
		t.Errorf("second run uploaded %d more blobs, want none: %v", len(blobPuts)-len(firstBlobPuts), blobPuts[len(firstBlobPuts):])
	}
	if len(mounts) != len(firstMounts) {
		t.Errorf("second run mounted %d more blobs, want none: %v", len(mounts)-len(firstMounts), mounts[len(firstMounts):])
	}
	if len(manifestPuts) != len(firstManifestPuts) {
		t.Errorf("second run wrote %d more manifests, want none: %v", len(manifestPuts)-len(firstManifestPuts), manifestPuts[len(firstManifestPuts):])
	}
}

// TestDeduplicatedPushMountsFromAPresentSibling is the incremental deploy, and the
// best case for the existence check: two of three services are already in the
// registry, so the third's shared layers are cross-mounted out of one of them and
// nothing is uploaded for them at all.
func TestDeduplicatedPushMountsFromAPresentSibling(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 3, 2)

	// First deploy everything but service-0, so its siblings are present.
	siblings := dm
	siblings.Operations = dm.Operations[1:]
	if err := runDedupDeploy(t, reg, layoutDirs, siblings); err != nil {
		t.Fatalf("deploying the siblings: %v", err)
	}
	before, _, _ := reg.snapshot()

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}

	// The shared layers were never uploaded again: service-1's manifest proves they
	// are in team/service-1, so they are mounted from there.
	added, mounts, _ := reg.snapshot()
	for _, digest := range images.shared {
		if got := reg.countBlobPuts(digest); got != 1 {
			t.Errorf("shared layer %s uploaded %d times in total, want the 1 from the first deploy", digest, got)
		}
		if !reg.hasBlob(images.repositories[0], digest) {
			t.Errorf("shared layer %s never reached %s", digest, images.repositories[0])
		}
	}
	// service-0 was absent from the first deploy, so every mount into it came from
	// the second one: one per shared layer, and nothing else.
	into := 0
	for _, mount := range mounts {
		if strings.HasPrefix(mount, images.repositories[0]+"@") {
			into++
		}
	}
	if into != len(images.shared) {
		t.Errorf("%d blobs were mounted into %s, want the %d shared layers: %v",
			into, images.repositories[0], len(images.shared), mounts)
	}
	sibling := images.repositories[1] // team/service-1, first alphabetically of the present ones
	if !reg.hasBlob(sibling, images.shared[0]) {
		t.Fatalf("the mount source %s does not hold the shared layer", sibling)
	}
	// The only new uploads are service-0's own layer and its config.
	if len(added)-len(before) != 2 {
		t.Errorf("second deploy uploaded %d blobs, want 2 (service-0's own layer and config): %v",
			len(added)-len(before), added[len(before):])
	}
}

// TestDeduplicatedPushFailsWhenMountsAreUnsupported verifies the mount-only
// contract: when the registry refuses to cross-mount a blob this deploy uploaded
// to its home repository, the push fails loudly instead of falling back to
// uploading it into every repository.
func TestDeduplicatedPushFailsWhenMountsAreUnsupported(t *testing.T) {
	reg := newNaiveRegistry()
	reg.mountable = false
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 2, 1)

	err := runDedupDeploy(t, reg, layoutDirs, dm)
	if err == nil {
		t.Fatal("deduplicated push succeeded against a registry without mount support, want a mount-only failure")
	}
	if !strings.Contains(err.Error(), "refusing to upload layer") {
		t.Fatalf("error = %v, want the mount-only refusal", err)
	}
	if !strings.Contains(err.Error(), images.repositories[0]) {
		t.Errorf("error = %v, want it to name the home repository %s", err, images.repositories[0])
	}
}

// TestDeduplicatedPushMountsUpstreamLayersInsteadOfUploading verifies that a layer
// already present in the destination registry -- recorded per layer when a shallow
// base image was pulled -- is mounted from there rather than downloaded and
// uploaded again.
func TestDeduplicatedPushMountsUpstreamLayersInsteadOfUploading(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 2, 1)

	// Record the shared layer as already available from library/base, and put it
	// there, as a pulled shallow base image would have.
	base := images.shared[0]
	content := []byte("shared-layer-0")
	reg.putBlob("library/base", base, content)
	dm = withLayerSource(t, dm, base, api.LayerSource{Registry: images.registry, Repository: "library/base"})

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}

	if got := reg.countBlobPuts(base); got != 0 {
		t.Errorf("the base layer was uploaded %d times, want 0 (it is mounted from library/base)", got)
	}
	for _, repository := range images.repositories {
		if !reg.hasBlob(repository, base) {
			t.Errorf("the base layer never reached %s", repository)
		}
	}
	// The services' own layers are uploaded as usual.
	for _, digest := range images.unique {
		if got := reg.countBlobPuts(digest); got != 1 {
			t.Errorf("layer %s uploaded %d times, want exactly 1", digest, got)
		}
	}
}

// withoutDedup returns dm with deduplicated_push turned off on the operation at the
// given index, so a test can build a deploy that mixes destinations which
// cross-mount blobs with one that does not.
func withoutDedup(t *testing.T, dm api.DeployManifest, index int) api.DeployManifest {
	t.Helper()
	out := dm
	out.Operations = nil
	for i, raw := range dm.Operations {
		if i == index {
			var op api.PushDeployOperation
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Fatalf("unmarshalling operation: %v", err)
			}
			op.DeduplicatedPush = false
			patched, err := json.Marshal(op)
			if err != nil {
				t.Fatalf("marshalling operation: %v", err)
			}
			raw = patched
		}
		out.Operations = append(out.Operations, raw)
	}
	return out
}

// withMirroredRegistry returns dm with every push operation duplicated against a
// second registry, as a multi_deploy that mirrors the same repositories to two
// registries produces.
func withMirroredRegistry(t *testing.T, dm api.DeployManifest, registry string) api.DeployManifest {
	t.Helper()
	out := dm
	for _, raw := range dm.Operations {
		var op api.PushDeployOperation
		if err := json.Unmarshal(raw, &op); err != nil {
			t.Fatalf("unmarshalling operation: %v", err)
		}
		op.Registry = registry
		mirrored, err := json.Marshal(op)
		if err != nil {
			t.Fatalf("marshalling operation: %v", err)
		}
		out.Operations = append(out.Operations, mirrored)
	}
	return out
}

// withLayerSource returns dm with the given layer source recorded on every
// occurrence of digest, mimicking what the build records for a layer pulled from a
// shallow base image.
func withLayerSource(t *testing.T, dm api.DeployManifest, digest string, source api.LayerSource) api.DeployManifest {
	t.Helper()
	out := dm
	out.Operations = nil
	for _, raw := range dm.Operations {
		var op api.PushDeployOperation
		if err := json.Unmarshal(raw, &op); err != nil {
			t.Fatalf("unmarshalling operation: %v", err)
		}
		for i := range op.Manifests {
			for j := range op.Manifests[i].LayerBlobs {
				if op.Manifests[i].LayerBlobs[j].Digest == digest {
					op.Manifests[i].LayerBlobs[j].Sources = []api.LayerSource{source}
				}
			}
		}
		patched, err := json.Marshal(op)
		if err != nil {
			t.Fatalf("marshalling operation: %v", err)
		}
		out.Operations = append(out.Operations, patched)
	}
	return out
}

// TestDeduplicatedPushIgnoredForASink runs the real one-shot entry point with
// deduplicated_push enabled in the deploy manifest and a sink on the command line.
// A sink captures the operations locally, so there is nothing to deduplicate: the
// setting has to be ignored rather than rejected, and the sink has to receive the
// blobs' actual bytes -- a mount-only view would have nothing to write.
func TestDeduplicatedPushIgnoredForASink(t *testing.T) {
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 2, 1)
	if ops, err := dm.PushOperations(); err != nil || len(ops) == 0 || !ops[0].DeduplicatedPush {
		t.Fatal("the fixture should have deduplicated_push enabled on its operations")
	}
	rawRequest, err := json.Marshal(dm)
	if err != nil {
		t.Fatalf("marshalling the deploy manifest: %v", err)
	}

	tarPath := filepath.Join(t.TempDir(), "out.tar")
	if err := DeployWithExtras(context.Background(), rawRequest, DeployOptions{
		OCILayouts: layoutDirs,
		Jobs:       4,
		Sink:       "oci-tar:" + tarPath,
	}); err != nil {
		t.Fatalf("deploying to a sink with deduplicated_push enabled: %v", err)
	}

	data, err := os.ReadFile(tarPath)
	if err != nil {
		t.Fatalf("reading the sink tarball: %v", err)
	}
	names := tarNames(t, data)
	// Every blob's bytes reached the tarball, including the layer both services
	// share: nothing was replaced by a cross-mount reference.
	for _, digest := range append(append([]string{}, images.shared...), images.unique...) {
		_, hexDigest, _ := strings.Cut(digest, ":")
		if entry := "blobs/sha256/" + hexDigest; !names[entry] {
			t.Errorf("%s is missing from the sink tarball", entry)
		}
	}
}
