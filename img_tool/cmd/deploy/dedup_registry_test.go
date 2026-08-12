package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
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

	// sharesBlobsAfterManifest models the other way a registry shares blobs between
	// repositories: not by mounting, but by exposing a blob to every repository once
	// some manifest references it (JFrog Artifactory behaves this way). With it on, a
	// blob's own HEAD answers 200 in a repository it was never uploaded to, which is
	// what deduplicated_push_content=blobs_and_artificial_manifests is for.
	sharesBlobsAfterManifest bool
	// requireBlobsForManifest rejects a manifest whose config or layers the registry
	// cannot see, the way a registry validating a manifest PUT does. It is what holds
	// an artificial manifest to being written after the blobs it references.
	requireBlobsForManifest bool
	// allBlobs holds every blob ever stored, by digest, so a blob shared by a manifest
	// reference can be served from a repository it was not uploaded to.
	allBlobs map[string][]byte
	// referenced holds the digests some stored manifest references.
	referenced map[string]struct{}

	// refuseUploads holds the repositories that answer an upload session with 401,
	// modelling a credential that may not write there. A deploy is only ever asked to
	// upload into a repository it does not push to by the process-wide blob location
	// cache, when it joins another deploy's claim (see blobLocations).
	refuseUploads map[string]struct{}

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
		blobs:      map[string]map[string][]byte{},
		manifests:  map[string]map[string]storedManifest{},
		uploads:    map[string][]byte{},
		allBlobs:   map[string][]byte{},
		referenced: map[string]struct{}{},
		mountable:  true,
	}
}

// newArtifactoryLikeRegistry returns a registry that refuses every cross-repository
// mount but shares a blob with every repository once a manifest references it, and
// validates the manifests it is given. It is the registry
// deduplicated_push_content=blobs_and_artificial_manifests exists for.
func newArtifactoryLikeRegistry() *naiveRegistry {
	r := newNaiveRegistry()
	r.mountable = false
	r.sharesBlobsAfterManifest = true
	r.requireBlobsForManifest = true
	return r
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

	if _, refused := r.refuseUploads[repository]; refused {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

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
	content, found := r.blobVisibleLocked(repository, digest)
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
		if missing := r.missingManifestBlobsLocked(repository, body); missing != "" {
			r.mu.Unlock()
			http.Error(w, "blob unknown to registry: "+missing, http.StatusBadRequest)
			return
		}
		if r.manifests[repository] == nil {
			r.manifests[repository] = map[string]storedManifest{}
		}
		r.manifests[repository][digest] = stored
		if reference != digest {
			r.manifests[repository][reference] = stored
		}
		r.referenceManifestBlobsLocked(body)
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
	r.allBlobs[digest] = content
}

// blobVisibleLocked returns the blob as the given repository sees it: the copy stored
// there, or -- on a registry that shares blobs a manifest references -- any copy at
// all, once some manifest references the digest.
func (r *naiveRegistry) blobVisibleLocked(repository, digest string) ([]byte, bool) {
	if content, found := r.blobs[repository][digest]; found {
		return content, true
	}
	if !r.sharesBlobsAfterManifest {
		return nil, false
	}
	if _, referenced := r.referenced[digest]; !referenced {
		return nil, false
	}
	content, found := r.allBlobs[digest]
	return content, found
}

// manifestBlobDigests returns the digests a manifest body references: its config and
// its layers. An index (or anything else this cannot parse as an image manifest)
// references no blobs of its own.
func manifestBlobDigests(body []byte) []string {
	var manifest registryv1.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil
	}
	if len(manifest.Layers) == 0 {
		return nil
	}
	digests := []string{manifest.Config.Digest.String()}
	for _, layer := range manifest.Layers {
		digests = append(digests, layer.Digest.String())
	}
	return digests
}

// missingManifestBlobsLocked returns the first blob a manifest references that the
// repository cannot see, or "" when it can see all of them. It is what makes a
// manifest PUT depend on the uploads that precede it.
func (r *naiveRegistry) missingManifestBlobsLocked(repository string, body []byte) string {
	if !r.requireBlobsForManifest {
		return ""
	}
	for _, digest := range manifestBlobDigests(body) {
		if _, found := r.blobVisibleLocked(repository, digest); !found {
			return digest
		}
	}
	return ""
}

// referenceManifestBlobsLocked records the blobs a stored manifest references, which
// is what makes them visible from every repository on a registry that shares them
// that way.
func (r *naiveRegistry) referenceManifestBlobsLocked(body []byte) {
	for _, digest := range manifestBlobDigests(body) {
		r.referenced[digest] = struct{}{}
	}
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

// blobPutRepositories returns the repositories the blob's bytes were uploaded to,
// sorted and without duplicates. One entry means every other repository that has the
// blob cross-mounted it.
func (r *naiveRegistry) blobPutRepositories(digest string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var repositories []string
	for _, put := range r.blobPuts {
		repository, uploaded, found := strings.Cut(put, "@")
		if !found || uploaded != digest {
			continue
		}
		if !slices.Contains(repositories, repository) {
			repositories = append(repositories, repository)
		}
	}
	sort.Strings(repositories)
	return repositories
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
		unique := []byte(fmt.Sprintf("service-%d-layer", service))
		contents := append(append([][]byte{}, sharedContent...), unique)

		// A real image config, because the artificial manifests of
		// deduplicated_push_content=blobs_and_artificial_manifests take each layer's
		// diff id from here. Nothing in these tests decompresses a layer, so the
		// fixture simply records each layer's own digest as its diff id.
		diffIDs := make([]string, len(contents))
		for i, content := range contents {
			diffIDs[i] = sha256Hash(content).String()
		}
		encodedDiffIDs, err := json.Marshal(diffIDs)
		if err != nil {
			t.Fatalf("marshalling diff ids: %v", err)
		}
		config := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","service":%d,"rootfs":{"type":"layers","diff_ids":%s}}`, service, encodedDiffIDs))

		src := ocilayout.NewMemBlobSource()
		src.Add(sha256Hash(config).Hex, config).Add(sha256Hash(unique).Hex, unique)
		images.blobs[sha256Hash(config).Hex] = config
		images.blobs[sha256Hash(unique).Hex] = unique
		descriptors := make([]registryv1.Descriptor, 0, sharedLayers+1)
		layerBlobs := make([]api.LayerBlob, 0, sharedLayers+1)
		for _, content := range contents {
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
				DeduplicatedPush: api.DeduplicatedPushEnabled,
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
	return runDedupDeployWith(t, transport, layoutDirs, dm, newBlobLocations(false))
}

// runDedupDeployWith is runDedupDeployVia against a given blob location cache, so a
// test can play several deploys through one cache the way the persistent worker
// handles several work requests.
func runDedupDeployWith(t *testing.T, transport http.RoundTripper, layoutDirs []string, dm api.DeployManifest, locations *blobLocations) error {
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
		locations:      locations,
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

// workRequest returns dm with only the operation at the given index, which is the
// shape a persistent worker sees: one deploy manifest per work request, planned on
// its own, with no way to know what the next request will push.
func workRequest(dm api.DeployManifest, index int) api.DeployManifest {
	out := dm
	out.Operations = dm.Operations[index : index+1]
	return out
}

// TestDeduplicatedPushMountsFromAnEarlierWorkRequestsHome is the persistent worker,
// one request at a time: two services that share their base layers are deployed by
// two work requests, each of which only knows its own image. The process-wide
// location cache is what makes the second one mount the shared layers out of the
// first one's repository instead of uploading its own copy -- and the control below
// shows that without it, it does.
func TestDeduplicatedPushMountsFromAnEarlierWorkRequestsHome(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 2, 2)
	locations := newBlobLocations(true)

	for i := range images.repositories {
		if err := runDedupDeployWith(t, reg.transport(), layoutDirs, workRequest(dm, i), locations); err != nil {
			t.Fatalf("work request %d: %v", i, err)
		}
	}
	reg.assertNoCrossRegistryMounts(t)

	home := images.repositories[0] // the first request's repository: it settled the home
	for _, digest := range images.shared {
		if got := reg.countBlobPuts(digest); got != 1 {
			t.Errorf("shared layer %s uploaded %d times across the two work requests, want exactly 1", digest, got)
		}
		if !reg.hasBlob(home, digest) {
			t.Errorf("shared layer %s is not in the home repository %s", digest, home)
		}
		if !reg.hasBlob(images.repositories[1], digest) {
			t.Errorf("shared layer %s never reached %s", digest, images.repositories[1])
		}
	}
	// One cross-mount per shared layer, into the second request's repository.
	_, mounts, _ := reg.snapshot()
	if len(mounts) != len(images.shared) {
		t.Errorf("registry satisfied %d mounts, want one per shared layer (%d): %v", len(mounts), len(images.shared), mounts)
	}
	// Each service's own layer is still uploaded to its own repository.
	for i, digest := range images.unique {
		if got := reg.countBlobPuts(digest); got != 1 {
			t.Errorf("layer %s uploaded %d times, want exactly 1", digest, got)
		}
		if !reg.hasBlob(images.repositories[i], digest) {
			t.Errorf("layer %s never reached %s", digest, images.repositories[i])
		}
	}
	for i, repository := range images.repositories {
		if _, found := reg.storedManifestFor(repository, images.manifests[i]); !found {
			t.Errorf("manifest %s never reached %s", images.manifests[i], repository)
		}
	}

	// The control: the same two work requests without a shared cache -- each planning
	// its own deploy, as a one-shot img deploy does -- upload every shared layer twice.
	// A single-image deploy has nothing to cross-mount into, so on its own the strategy
	// cannot help here at all.
	control := newNaiveRegistry()
	for i := range images.repositories {
		if err := runDedupDeployVia(t, control.transport(), layoutDirs, workRequest(dm, i)); err != nil {
			t.Fatalf("control work request %d: %v", i, err)
		}
	}
	for _, digest := range images.shared {
		if got := control.countBlobPuts(digest); got != 2 {
			t.Errorf("without the shared cache, shared layer %s was uploaded %d times, want once per work request (2)", digest, got)
		}
	}
}

// TestDeduplicatedPushConcurrentWorkRequestsShareOneHome is the concurrent worker:
// several work requests are planned at the same time, so none of them can see
// another's finished upload. They must still agree on one home per shared blob, and
// the blob's bytes must cross the wire exactly once -- each request pushes through a
// remote.Pusher of its own, so nothing below the location cache would keep them from
// all uploading it.
func TestDeduplicatedPushConcurrentWorkRequestsShareOneHome(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 3, 2)
	locations := newBlobLocations(true)

	errs := make([]error, len(images.repositories))
	var wg sync.WaitGroup
	for i := range images.repositories {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = runDedupDeployWith(t, reg.transport(), layoutDirs, workRequest(dm, i), locations)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent work request %d: %v", i, err)
		}
	}
	reg.assertNoCrossRegistryMounts(t)

	for _, digest := range images.shared {
		// One home, whichever request got there first, and one upload into it: the
		// requests that did not perform it either waited for the one that did or found
		// the blob already there.
		if repositories := reg.blobPutRepositories(digest); len(repositories) != 1 {
			t.Errorf("shared layer %s was uploaded to %v, want a single home repository", digest, repositories)
		}
		if got := reg.countBlobPuts(digest); got != 1 {
			t.Errorf("shared layer %s uploaded %d times, want exactly 1 across the concurrent requests", digest, got)
		}
		for _, repository := range images.repositories {
			if !reg.hasBlob(repository, digest) {
				t.Errorf("shared layer %s never reached %s", digest, repository)
			}
		}
	}
	for i, repository := range images.repositories {
		if _, found := reg.storedManifestFor(repository, images.manifests[i]); !found {
			t.Errorf("manifest %s never reached %s", images.manifests[i], repository)
		}
	}
}

// TestDeduplicatedPushLeavesARefusedJoinedUploadToTheOrdinaryPush covers the one
// thing the location cache asks of a deploy that it did not ask for itself: uploading
// a blob to a repository it does not push to, because a concurrent deploy claimed it
// as the home. A credential that may not write there must not fail the deploy -- it
// gives up the cross-mount for that blob and uploads the bytes into its own
// repository, which is what would have happened without the cache.
func TestDeduplicatedPushLeavesARefusedJoinedUploadToTheOrdinaryPush(t *testing.T) {
	reg := newNaiveRegistry()
	reg.refuseUploads = map[string]struct{}{"other/home": {}}
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 1, 1)
	shared := images.shared[0]

	// A concurrent deploy in this process claimed a home this one cannot write to.
	locations := newBlobLocations(true)
	locations.inflight[blobKey{registry: "reg.example.com", digest: shared}] = blobMount{repository: "other/home", kind: mountFromUpload}

	if err := runDedupDeployWith(t, reg.transport(), layoutDirs, dm, locations); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}
	reg.assertNoCrossRegistryMounts(t)

	if reg.hasBlob("other/home", shared) {
		t.Error("the shared layer reached the home repository, which refuses uploads")
	}
	if !reg.hasBlob(images.repositories[0], shared) {
		t.Errorf("the shared layer never reached %s, so the deploy pushed an unservable manifest", images.repositories[0])
	}
	if got := reg.countBlobPuts(shared); got != 1 {
		t.Errorf("the shared layer's bytes were uploaded %d times, want 1 (into its own repository)", got)
	}
	if _, found := reg.storedManifestFor(images.repositories[0], images.manifests[0]); !found {
		t.Errorf("manifest %s never reached %s", images.manifests[0], images.repositories[0])
	}
}

// TestDeduplicatedPushFailsWhenItsOwnHomeRefusesTheUpload is the other half of the
// pair: an upload this deploy chose to make is its own work, so failing it fails the
// deploy rather than being papered over.
func TestDeduplicatedPushFailsWhenItsOwnHomeRefusesTheUpload(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 2, 1)
	reg.refuseUploads = map[string]struct{}{images.repositories[0]: {}} // the home

	err := runDedupDeploy(t, reg, layoutDirs, dm)
	if err == nil {
		t.Fatal("deduplicated push succeeded although the home repository refused the upload")
	}
	if !strings.Contains(err.Error(), "uploading shared blobs") {
		t.Errorf("error = %v, want the upload phase to name itself", err)
	}
	if !strings.Contains(err.Error(), images.shared[0]) {
		t.Errorf("error = %v, want it to name the shared layer %s", err, images.shared[0])
	}
}

// TestDeduplicatedPushFailsWhenAnEarlierHomeCannotBeMounted pins the trust the
// location cache carries across work requests: the home is a repository *this
// process* uploaded the blob to, so the request that mounts from it fails loudly if
// the registry refuses, exactly as it would for a home of its own choosing. A silent
// fallback would upload the blob into every repository after having already uploaded
// it to the home -- strictly worse than never enabling the strategy.
func TestDeduplicatedPushFailsWhenAnEarlierHomeCannotBeMounted(t *testing.T) {
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 2, 1)
	locations := newBlobLocations(true)

	// The first work request settles the home and puts the shared layer in it.
	if err := runDedupDeployWith(t, reg.transport(), layoutDirs, workRequest(dm, 0), locations); err != nil {
		t.Fatalf("first work request: %v", err)
	}
	home := images.repositories[0]
	if !reg.hasBlob(home, images.shared[0]) {
		t.Fatalf("the shared layer is not in the home repository %s, so this test proves nothing", home)
	}

	// The second one has nothing to upload for that layer -- and no way to push it
	// either, once the registry stops mounting.
	reg.mountable = false
	err := runDedupDeployWith(t, reg.transport(), layoutDirs, workRequest(dm, 1), locations)
	if err == nil {
		t.Fatal("the second work request succeeded against a registry without mount support, want a mount-only failure")
	}
	if !strings.Contains(err.Error(), "refusing to upload layer") {
		t.Fatalf("error = %v, want the mount-only refusal", err)
	}
	if !strings.Contains(err.Error(), home) {
		t.Errorf("error = %v, want it to name the home repository %s the first request uploaded to", err, home)
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
			op.DeduplicatedPush = ""
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
	if ops, err := dm.PushOperations(); err != nil || len(ops) == 0 || ops[0].DeduplicatedPush != api.DeduplicatedPushEnabled {
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

// withDedupSettingsOnEveryOp returns dm with the given deduplicated push settings on
// every push operation, as the global flags (or every target's attributes) produce.
// An empty mode leaves each operation's own value alone.
func withDedupSettingsOnEveryOp(t *testing.T, dm api.DeployManifest, mode, home, content string) api.DeployManifest {
	t.Helper()
	out := dm
	out.Operations = nil
	for _, raw := range dm.Operations {
		var op api.PushDeployOperation
		if err := json.Unmarshal(raw, &op); err != nil {
			t.Fatalf("unmarshalling operation: %v", err)
		}
		if mode != "" {
			op.DeduplicatedPush = mode
		}
		op.DeduplicatedPushBlobRepository = home
		op.DeduplicatedPushContent = content
		patched, err := json.Marshal(op)
		if err != nil {
			t.Fatalf("marshalling operation: %v", err)
		}
		out.Operations = append(out.Operations, patched)
	}
	return out
}

// artificialManifestsIn returns the manifests a repository holds that this deploy
// wrote only to reference a blob, keyed by the digest they reference.
func (r *naiveRegistry) artificialManifestsIn(t *testing.T, repository string) map[string]registryv1.Manifest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]registryv1.Manifest{}
	for identifier, stored := range r.manifests[repository] {
		var manifest registryv1.Manifest
		if err := json.Unmarshal(stored.body, &manifest); err != nil {
			continue
		}
		referenced, artificial := manifest.Annotations[artificialManifestAnnotation]
		if !artificial {
			continue
		}
		if identifier != stored.digest {
			t.Errorf("artificial manifest for %s is tagged %q, want it pushed by digest only", referenced, identifier)
		}
		out[referenced] = manifest
	}
	return out
}

// TestDeduplicatedPushArtificialManifestsShareBlobsWithoutMounting is the registry
// deduplicated_push_content=blobs_and_artificial_manifests exists for: one that
// refuses every cross-repository mount, but shares a blob with every repository once
// a manifest references it. The shared layers are uploaded once, referenced from a
// manifest in their home repository, and found by every other repository's own blob
// check -- so no mount is needed and nothing is uploaded twice.
func TestDeduplicatedPushArtificialManifestsShareBlobsWithoutMounting(t *testing.T) {
	reg := newArtifactoryLikeRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 3, 2)
	dm = withDedupSettingsOnEveryOp(t, dm, api.DeduplicatedPushEnabled, "", api.DeduplicatedPushContentBlobsAndArtificialManifests)

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}

	home := images.repositories[0]
	artificial := reg.artificialManifestsIn(t, home)
	if len(artificial) != len(images.shared) {
		t.Fatalf("home repository %s holds %d artificial manifests, want one per shared layer (%d)", home, len(artificial), len(images.shared))
	}
	for i, digest := range images.shared {
		// One upload, into the home repository, and no mounts anywhere.
		if got := reg.blobPutRepositories(digest); len(got) != 1 || got[0] != home {
			t.Errorf("shared layer %d was uploaded to %v, want only the home repository %s", i, got, home)
		}
		manifest, found := artificial[digest]
		if !found {
			t.Fatalf("no artificial manifest references shared layer %d (%s)", i, digest)
		}
		if len(manifest.Layers) != 1 || manifest.Layers[0].Digest.String() != digest {
			t.Errorf("artificial manifest layers = %+v, want just the shared layer %s", manifest.Layers, digest)
		}
		// The manifest is only a reference if the registry can resolve what it points
		// at: both blobs have to be in the home repository.
		if !reg.hasBlob(home, manifest.Config.Digest.String()) {
			t.Errorf("the artificial config %s is not in %s", manifest.Config.Digest, home)
		}
		// Every other repository sees the blob without holding a copy of its own.
		for _, repository := range images.repositories[1:] {
			if reg.hasBlob(repository, digest) {
				t.Errorf("%s holds its own copy of shared layer %d, want it shared from %s", repository, i, home)
			}
		}
	}
	if _, mounts, _ := reg.snapshot(); len(mounts) != 0 {
		t.Errorf("registry satisfied %v mounts, want none: this registry does not mount at all", mounts)
	}
	// Every image is pushed, so every real manifest is there -- which this registry
	// only accepts once it can see the blobs the manifest references.
	for i, repository := range images.repositories {
		if _, found := reg.storedManifestFor(repository, images.manifests[i]); !found {
			t.Errorf("%s does not hold its manifest %s", repository, images.manifests[i])
		}
	}
}

// TestDeduplicatedPushArtificialConfigRecordsTheDiffID checks the config blob of an
// artificial manifest against the image config the layer really belongs to: a
// registry that insists on a manifest before sharing a blob deserves a real one.
func TestDeduplicatedPushArtificialConfigRecordsTheDiffID(t *testing.T) {
	reg := newArtifactoryLikeRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 2, 1)
	dm = withDedupSettingsOnEveryOp(t, dm, api.DeduplicatedPushEnabled, "", api.DeduplicatedPushContentBlobsAndArtificialManifests)

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}

	home := images.repositories[0]
	shared := images.shared[0]
	manifest, found := reg.artificialManifestsIn(t, home)[shared]
	if !found {
		t.Fatalf("no artificial manifest references the shared layer %s", shared)
	}
	raw, found := reg.blobContent(home, manifest.Config.Digest.String())
	if !found {
		t.Fatalf("the artificial config %s is not in %s", manifest.Config.Digest, home)
	}
	config, err := registryv1.ParseConfigFile(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parsing the artificial config %s: %v", manifest.Config.Digest, err)
	}
	// The fixture's configs record each layer's own digest as its diff id, so that is
	// what the artificial config must have picked up.
	if len(config.RootFS.DiffIDs) != 1 || config.RootFS.DiffIDs[0].String() != shared {
		t.Errorf("artificial config diff ids = %v, want just the shared layer's uncompressed digest %s", config.RootFS.DiffIDs, shared)
	}
	if config.OS != "unknown" || config.Architecture != "unknown" {
		t.Errorf("artificial config platform = %s/%s, want unknown/unknown: it is not a runnable image", config.OS, config.Architecture)
	}
	if manifest.MediaType != types.OCIManifestSchema1 || manifest.Config.MediaType != types.OCIConfigJSON {
		t.Errorf("artificial manifest media types = %s / %s, want the OCI pair matching the OCI layers", manifest.MediaType, manifest.Config.MediaType)
	}
}

// TestDeduplicatedPushWithoutArtificialManifestsFailsOnSuchARegistry is the control
// for the two tests above: on the same registry, uploading the blobs alone leaves
// them invisible to every other repository, and the strict mount fails the push
// rather than uploading them everywhere.
func TestDeduplicatedPushWithoutArtificialManifestsFailsOnSuchARegistry(t *testing.T) {
	reg := newArtifactoryLikeRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 2, 1)
	dm = withDedupSettingsOnEveryOp(t, dm, api.DeduplicatedPushEnabled, "", api.DeduplicatedPushContentBlobs)

	err := runDedupDeploy(t, reg, layoutDirs, dm)
	if err == nil {
		t.Fatal("the push succeeded without an artificial manifest, want the mount-only failure")
	}
	if !strings.Contains(err.Error(), "refusing to upload layer") {
		t.Fatalf("error = %v, want the mount-only refusal", err)
	}
	if !strings.Contains(err.Error(), images.repositories[0]) {
		t.Errorf("error = %v, want it to name the home repository %s", err, images.repositories[0])
	}
}

// TestDeduplicatedPushBestEffortFallsBackToUploading is the best_effort mode against
// a registry that shares blobs no way at all: the deduplication is attempted, every
// mount is refused, and the layer's bytes are uploaded the ordinary way instead of
// failing the deploy the way TestDeduplicatedPushFailsWhenMountsAreUnsupported does.
// The cost is the upload the strategy meant to avoid -- which is the trade the mode is.
func TestDeduplicatedPushBestEffortFallsBackToUploading(t *testing.T) {
	reg := newNaiveRegistry()
	reg.mountable = false
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 3, 1)
	dm = withDedupSettingsOnEveryOp(t, dm, api.DeduplicatedPushBestEffort, "", api.DeduplicatedPushContentBlobs)

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("best-effort deduplicated push: %v", err)
	}
	// Every repository ends up with the layer, and every image is pushed.
	if got := reg.blobPutRepositories(images.shared[0]); len(got) != len(images.repositories) {
		t.Errorf("the shared layer was uploaded to %v, want every repository once the mount was refused", got)
	}
	for i, repository := range images.repositories {
		if _, found := reg.storedManifestFor(repository, images.manifests[i]); !found {
			t.Errorf("%s does not hold its manifest %s", repository, images.manifests[i])
		}
	}
}

// TestDeduplicatedPushPinsTheBlobRepository covers
// deduplicated_push_blob_repository: every blob shared between repositories is
// uploaded to the named repository and cross-mounted from there, and no destination
// repository is ever uploaded a layer of its own.
func TestDeduplicatedPushPinsTheBlobRepository(t *testing.T) {
	const pinned = "team/_blobs"
	reg := newNaiveRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 3, 2)
	dm = withDedupSettingsOnEveryOp(t, dm, api.DeduplicatedPushEnabled, pinned, "")

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}

	// Both the shared layers and each service's own layer live in the pinned
	// repository: a home this deploy uploads to is a home wherever the blob is headed.
	for _, digest := range append(append([]string{}, images.shared...), images.unique...) {
		if got := reg.blobPutRepositories(digest); len(got) != 1 || got[0] != pinned {
			t.Errorf("layer %s was uploaded to %v, want only the pinned repository %s", digest, got, pinned)
		}
	}
	_, mounts, _ := reg.snapshot()
	for i, repository := range images.repositories {
		for _, digest := range append([]string{images.unique[i]}, images.shared...) {
			if !slices.Contains(mounts, repository+"@"+digest) {
				t.Errorf("%s did not cross-mount %s, want it mounted out of %s", repository, digest, pinned)
			}
		}
		if _, found := reg.storedManifestFor(repository, images.manifests[i]); !found {
			t.Errorf("%s does not hold its manifest %s", repository, images.manifests[i])
		}
	}
}

// TestDeduplicatedPushArtificialManifestsAreWrittenOnce plays two work requests
// through one location cache, as the persistent worker does: the second request
// mounts the shared layer out of the home the first one filled, and writes no second
// artificial manifest for it.
func TestDeduplicatedPushArtificialManifestsAreWrittenOnce(t *testing.T) {
	reg := newArtifactoryLikeRegistry()
	layoutDirs, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 2, 1)
	dm = withDedupSettingsOnEveryOp(t, dm, api.DeduplicatedPushEnabled, "", api.DeduplicatedPushContentBlobsAndArtificialManifests)
	locations := newBlobLocations(true)

	if err := runDedupDeployWith(t, reg.transport(), layoutDirs, workRequest(dm, 0), locations); err != nil {
		t.Fatalf("first work request: %v", err)
	}
	_, _, afterFirst := reg.snapshot()

	if err := runDedupDeployWith(t, reg.transport(), layoutDirs, workRequest(dm, 1), locations); err != nil {
		t.Fatalf("second work request: %v", err)
	}

	home := images.repositories[0]
	shared := images.shared[0]
	if got := reg.countBlobPuts(shared); got != 1 {
		t.Errorf("the shared layer's bytes were uploaded %d times, want once across both work requests", got)
	}
	// One manifest references the shared layer, in the home repository the first
	// request filled. (Each request also writes one for the layer only it needs, which
	// is what lets a later request share that one too.)
	if _, found := reg.artificialManifestsIn(t, home)[shared]; !found {
		t.Errorf("home repository %s holds no artificial manifest for the shared layer", home)
	}
	if _, found := reg.artificialManifestsIn(t, images.repositories[1])[shared]; found {
		t.Errorf("the second request wrote its own artificial manifest for the shared layer, want it shared from %s", home)
	}
	// The manifest was written by the first request, before the second one needed it.
	var referenced int
	for _, put := range afterFirst {
		if strings.HasPrefix(put, home+":") {
			referenced++
		}
	}
	if referenced < 2 {
		t.Errorf("the first work request wrote %d manifests to %s, want its own plus the artificial one", referenced, home)
	}
}

// TestDeduplicatedPushArtificialManifestsAreIdempotent re-runs the same deploy: the
// artificial manifests are derived from the blob and its diff id alone, so the second
// run finds everything in place and uploads nothing.
func TestDeduplicatedPushArtificialManifestsAreIdempotent(t *testing.T) {
	reg := newArtifactoryLikeRegistry()
	layoutDirs, dm, _ := buildSharedLayerLayouts(t, "reg.example.com", 2, 1)
	dm = withDedupSettingsOnEveryOp(t, dm, api.DeduplicatedPushEnabled, "", api.DeduplicatedPushContentBlobsAndArtificialManifests)

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("first deduplicated push: %v", err)
	}
	firstBlobPuts, _, firstManifestPuts := reg.snapshot()

	if err := runDedupDeploy(t, reg, layoutDirs, dm); err != nil {
		t.Fatalf("second deduplicated push: %v", err)
	}
	secondBlobPuts, _, secondManifestPuts := reg.snapshot()

	if len(secondBlobPuts) != len(firstBlobPuts) {
		t.Errorf("the second run uploaded %d blobs (first run: %d), want none: everything is already there",
			len(secondBlobPuts)-len(firstBlobPuts), len(firstBlobPuts))
	}
	// The manifests the deploy itself writes are re-tagged; the artificial ones are
	// pushed by digest, which go-containerregistry HEADs first and skips.
	if extra := len(secondManifestPuts) - len(firstManifestPuts); extra > len(firstManifestPuts) {
		t.Errorf("the second run wrote %d extra manifests, want at most the tagged ones again", extra)
	}
}

// blobContent returns the bytes a repository holds for a digest.
func (r *naiveRegistry) blobContent(repository, digest string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	content, found := r.blobs[repository][digest]
	return content, found
}
