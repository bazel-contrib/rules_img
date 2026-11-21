package referrers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// conditionalRegistry wraps the in-memory registry with what it lacks: ETags,
// conditional manifest writes, and (optionally) the OCI-Subject header. It
// serves one request at a time, so a conditional write is atomic.
type conditionalRegistry struct {
	inner http.Handler
	// subjectHeader sets OCI-Subject on writes of manifests with a subject.
	subjectHeader bool
	// rejectConditions fails every conditional write, as a registry that
	// doesn't evaluate the condition correctly might.
	rejectConditions bool

	mu sync.Mutex
	// tagWrites counts the writes of referrers tags, and rejected the ones that
	// failed their condition.
	tagWrites, rejected int
}

func newConditionalRegistry(referrersAPI bool) *conditionalRegistry {
	return &conditionalRegistry{inner: registry.New(registry.Logger(log.New(io.Discard, "", 0)), registry.WithReferrersSupport(referrersAPI))}
}

func (c *conditionalRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()

	manifest := strings.Contains(r.URL.Path, "/manifests/")
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if r.Method == http.MethodPut && manifest && strings.Contains(r.URL.Path, "/manifests/sha256-") {
		c.tagWrites++
	}
	ifMatch, ifNoneMatch := r.Header.Get("If-Match"), r.Header.Get("If-None-Match")
	if r.Method == http.MethodPut && manifest && (ifMatch != "" || ifNoneMatch != "") {
		head := httptest.NewRecorder()
		c.inner.ServeHTTP(head, httptest.NewRequest(http.MethodHead, r.URL.String(), nil))
		exists := head.Code == http.StatusOK
		failed := ifMatch != "" && (!exists || etagOf(head.Header()) != ifMatch) || ifNoneMatch == "*" && exists
		if c.rejectConditions || failed {
			c.rejected++
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
	}

	recorder := httptest.NewRecorder()
	c.inner.ServeHTTP(recorder, r)
	for k, v := range recorder.Header() {
		w.Header()[k] = v
	}
	if manifest && (r.Method == http.MethodGet || r.Method == http.MethodHead) && recorder.Code == http.StatusOK {
		w.Header().Set("ETag", etagOf(recorder.Header()))
	}
	if manifest && r.Method == http.MethodPut && recorder.Code == http.StatusCreated && c.subjectHeader {
		var m struct {
			Subject *v1.Descriptor `json:"subject"`
		}
		if json.Unmarshal(body, &m) == nil && m.Subject != nil {
			w.Header().Set(headerSubject, m.Subject.Digest.String())
		}
	}
	w.WriteHeader(recorder.Code)
	w.Write(recorder.Body.Bytes())
}

func etagOf(h http.Header) string { return `"` + h.Get("Docker-Content-Digest") + `"` }

const testHost = "registry.test"

func testRepo(t *testing.T) name.Repository {
	t.Helper()
	repo, err := name.NewRepository(testHost+"/test/image", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func newTestWriter(t *testing.T, handler http.Handler) *Writer {
	t.Helper()
	rt := handlerTransport{handler: handler}
	pusher, err := remote.NewPusher(remote.WithTransport(rt))
	if err != nil {
		t.Fatal(err)
	}
	return NewWriter(pusher, Options{Transport: rt, Remote: []remote.Option{remote.WithTransport(rt)}})
}

// pushSubject pushes an image to refer to, and returns its descriptor.
func pushSubject(t *testing.T, w *Writer, repo name.Repository) v1.Descriptor {
	t.Helper()
	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := partialDescriptor(image)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Push(context.Background(), repo.Digest(descriptor.Digest.String()), image); err != nil {
		t.Fatalf("pushing the subject: %v", err)
	}
	return descriptor
}

func partialDescriptor(image v1.Image) (v1.Descriptor, error) {
	digest, err := image.Digest()
	if err != nil {
		return v1.Descriptor{}, err
	}
	size, err := image.Size()
	if err != nil {
		return v1.Descriptor{}, err
	}
	mediaType, err := image.MediaType()
	if err != nil {
		return v1.Descriptor{}, err
	}
	return v1.Descriptor{MediaType: mediaType, Digest: digest, Size: size}, nil
}

// newReferrer returns an artifact (without an artifactType, so that its
// config's media type stands in) that refers to subject.
func newReferrer(t *testing.T, subject v1.Descriptor, marker string) v1.Image {
	t.Helper()
	layer := static.NewLayer([]byte(marker), "application/vnd.test.blob")
	image, err := mutate.AppendLayers(mutate.MediaType(empty.Image, types.OCIManifestSchema1), layer)
	if err != nil {
		t.Fatal(err)
	}
	image = mutate.ConfigMediaType(image, "application/vnd.test.config")
	image = mutate.Annotations(image, map[string]string{"marker": marker}).(v1.Image)
	return mutate.Subject(image, subject).(v1.Image)
}

func tagIndex(t *testing.T, handler http.Handler, repo name.Repository, subject v1.Descriptor) []v1.Descriptor {
	t.Helper()
	tag, err := TagSchemaTag(subject.Digest.String())
	if err != nil {
		t.Fatal(err)
	}
	index, err := remote.Index(repo.Tag(tag), remote.WithTransport(handlerTransport{handler: handler}))
	if err != nil {
		t.Fatalf("reading the referrers tag: %v", err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	return manifest.Manifests
}

func digests(descriptors []v1.Descriptor) []string {
	var out []string
	for _, d := range descriptors {
		out = append(out, d.Digest.String())
	}
	slices.Sort(out)
	return out
}

func TestWriterListsReferrerInTagSchema(t *testing.T) {
	handler := newConditionalRegistry(false)
	w := newTestWriter(t, handler)
	repo := testRepo(t)
	subject := pushSubject(t, w, repo)

	referrer := newReferrer(t, subject, "one")
	digest, err := referrer.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Push(context.Background(), repo.Digest(digest.String()), referrer); err != nil {
		t.Fatalf("Push() = %v", err)
	}

	listed := tagIndex(t, handler, repo, subject)
	if len(listed) != 1 {
		t.Fatalf("the referrers tag lists %v, want one referrer", listed)
	}
	got := listed[0]
	if got.Digest != digest || got.MediaType != types.OCIManifestSchema1 || got.ArtifactType != "application/vnd.test.config" || got.Annotations["marker"] != "one" {
		t.Errorf("the referrers tag lists %+v, want %s with the config's media type as its artifact type and the manifest's annotations", got, digest)
	}
	// The blobs are there, so the referrer can be pulled.
	if _, err := remote.Image(repo.Digest(digest.String()), remote.WithTransport(handlerTransport{handler: handler})); err != nil {
		t.Errorf("pulling the referrer: %v", err)
	}
	layers, _ := referrer.Layers()
	layerDigest, _ := layers[0].Digest()
	if _, err := remote.Layer(repo.Digest(layerDigest.String()), remote.WithTransport(handlerTransport{handler: handler})); err != nil {
		t.Errorf("pulling the referrer's layer: %v", err)
	}
}

// Writers that don't know of each other (as separate processes) push
// referrers of one subject at once: each adds its own to the referrers tag
// conditionally, so none is lost.
func TestWriterKeepsConcurrentReferrers(t *testing.T) {
	handler := newConditionalRegistry(false)
	repo := testRepo(t)
	subject := pushSubject(t, newTestWriter(t, handler), repo)

	var want []string
	var referrers []v1.Image
	for i := range 8 {
		referrer := newReferrer(t, subject, fmt.Sprint(i))
		digest, err := referrer.Digest()
		if err != nil {
			t.Fatal(err)
		}
		referrers = append(referrers, referrer)
		want = append(want, digest.String())
	}
	slices.Sort(want)

	var wg sync.WaitGroup
	errs := make([]error, len(referrers))
	for i, referrer := range referrers {
		w := newTestWriter(t, handler)
		wg.Go(func() {
			digest, _ := referrer.Digest()
			errs[i] = w.Push(context.Background(), repo.Digest(digest.String()), referrer)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("Push() = %v", err)
		}
	}

	if got := digests(tagIndex(t, handler, repo, subject)); !slices.Equal(got, want) {
		t.Errorf("the referrers tag lists %d of the %d referrers:\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
}

// A referrer the registry already has, but that its subject's referrers tag
// lost (to a push that raced it, say), is listed again when pushed again.
func TestWriterRestoresMissingEntry(t *testing.T) {
	handler := newConditionalRegistry(false)
	w := newTestWriter(t, handler)
	repo := testRepo(t)
	subject := pushSubject(t, w, repo)

	kept, lost := newReferrer(t, subject, "kept"), newReferrer(t, subject, "lost")
	for _, referrer := range []v1.Image{kept, lost} {
		digest, _ := referrer.Digest()
		if err := w.Push(context.Background(), repo.Digest(digest.String()), referrer); err != nil {
			t.Fatalf("Push() = %v", err)
		}
	}

	// Lose one: rewrite the tag with only the other.
	tag, _ := TagSchemaTag(subject.Digest.String())
	listed := tagIndex(t, handler, repo, subject)
	keptDigest, _ := kept.Digest()
	listed = slices.DeleteFunc(listed, func(d v1.Descriptor) bool { return d.Digest != keptDigest })
	raw, _ := json.Marshal(v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: listed})
	if err := remote.Put(repo.Tag(tag), rawManifest{raw: raw, mediaType: types.OCIImageIndex}, remote.WithTransport(handlerTransport{handler: handler})); err != nil {
		t.Fatal(err)
	}

	lostDigest, _ := lost.Digest()
	if err := newTestWriter(t, handler).Push(context.Background(), repo.Digest(lostDigest.String()), lost); err != nil {
		t.Fatalf("Push() = %v", err)
	}
	want := []string{keptDigest.String(), lostDigest.String()}
	slices.Sort(want)
	if got := digests(tagIndex(t, handler, repo, subject)); !slices.Equal(got, want) {
		t.Errorf("the referrers tag lists %v, want %v", got, want)
	}
}

// A registry that processed the subject and has the referrers API lists the
// referrer itself: no referrers tag.
func TestWriterTrustsReferrersAPI(t *testing.T) {
	handler := newConditionalRegistry(true)
	handler.subjectHeader = true
	w := newTestWriter(t, handler)
	repo := testRepo(t)
	subject := pushSubject(t, w, repo)

	referrer := newReferrer(t, subject, "one")
	digest, _ := referrer.Digest()
	if err := w.Push(context.Background(), repo.Digest(digest.String()), referrer); err != nil {
		t.Fatalf("Push() = %v", err)
	}
	if handler.tagWrites != 0 {
		t.Errorf("wrote the referrers tag %d times, want none", handler.tagWrites)
	}
	listed, source, err := List(context.Background(), repo, subject.Digest.String(), nil, Options{Transport: handlerTransport{handler: handler}})
	if err != nil {
		t.Fatal(err)
	}
	if source != SourceAPI || !slices.Equal(digests(listed), []string{digest.String()}) {
		t.Errorf("List() = %v from %s, want %s from the referrers API", digests(listed), source, digest)
	}
}

// Without the OCI-Subject header, the spec requires the client to maintain the
// referrers tag, even if the registry has the referrers API. And a registry
// that sends the header without having the API (as AWS ECR does) needs it too.
func TestWriterMaintainsTagUnlessBothConfirm(t *testing.T) {
	for _, tc := range []struct {
		name          string
		api, header   bool
		wantTagWrites bool
	}{
		{name: "no API, no header", wantTagWrites: true},
		{name: "API without header", api: true, wantTagWrites: true},
		{name: "header without API", header: true, wantTagWrites: true},
		{name: "API and header", api: true, header: true, wantTagWrites: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := newConditionalRegistry(tc.api)
			handler.subjectHeader = tc.header
			w := newTestWriter(t, handler)
			repo := testRepo(t)
			subject := pushSubject(t, w, repo)
			referrer := newReferrer(t, subject, "one")
			digest, _ := referrer.Digest()
			if err := w.Push(context.Background(), repo.Digest(digest.String()), referrer); err != nil {
				t.Fatalf("Push() = %v", err)
			}
			if got := handler.tagWrites > 0; got != tc.wantTagWrites {
				t.Errorf("wrote the referrers tag: %v, want %v", got, tc.wantTagWrites)
			}
		})
	}
}

// A registry that rejects a write conditional on the ETag of an index that then
// turns out not to have changed doesn't evaluate conditions correctly: the next
// write goes without a condition (and is checked by reading the tag back).
func TestWriterSurvivesBrokenConditions(t *testing.T) {
	handler := newConditionalRegistry(false)
	w := newTestWriter(t, handler)
	repo := testRepo(t)
	subject := pushSubject(t, w, repo)
	first, second := newReferrer(t, subject, "first"), newReferrer(t, subject, "second")
	firstDigest, _ := first.Digest()
	if err := w.Push(context.Background(), repo.Digest(firstDigest.String()), first); err != nil {
		t.Fatalf("Push() = %v", err)
	}

	handler.rejectConditions = true
	secondDigest, _ := second.Digest()
	if err := w.Push(context.Background(), repo.Digest(secondDigest.String()), second); err != nil {
		t.Fatalf("Push() = %v", err)
	}
	if handler.rejected != 1 {
		t.Errorf("the registry rejected %d conditional writes, want 1", handler.rejected)
	}
	want := []string{firstDigest.String(), secondDigest.String()}
	slices.Sort(want)
	if got := digests(tagIndex(t, handler, repo, subject)); !slices.Equal(got, want) {
		t.Errorf("the referrers tag lists %v, want %v", got, want)
	}
}

func TestWriterRejectsForeignTag(t *testing.T) {
	handler := newConditionalRegistry(false)
	w := newTestWriter(t, handler)
	repo := testRepo(t)
	subject := pushSubject(t, w, repo)

	// An image happens to be tagged like the subject's referrers tag.
	tag, _ := TagSchemaTag(subject.Digest.String())
	other, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(repo.Tag(tag), other, remote.WithTransport(handlerTransport{handler: handler})); err != nil {
		t.Fatal(err)
	}

	referrer := newReferrer(t, subject, "one")
	digest, _ := referrer.Digest()
	err = w.Push(context.Background(), repo.Digest(digest.String()), referrer)
	if err == nil || !strings.Contains(err.Error(), "not an image index") {
		t.Errorf("Push() = %v, want an error saying the tag is not an image index", err)
	}
}

// failFirstReferrersCheck answers the first request to the referrers API with
// status, as a registry does when a token lacks a scope or it is overloaded.
type failFirstReferrersCheck struct {
	http.Handler
	status int
	failed bool
}

func (f *failFirstReferrersCheck) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !f.failed && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/referrers/") {
		f.failed = true
		w.WriteHeader(f.status)
		return
	}
	f.Handler.ServeHTTP(w, r)
}

// An answer of the referrers API that may change (here: a 403) falls back to
// the referrers tag, but isn't remembered: the next referrer asks again, and
// trusts the API.
func TestWriterDoesNotRememberTransientAPIAnswer(t *testing.T) {
	inner := newConditionalRegistry(true)
	inner.subjectHeader = true
	w := newTestWriter(t, &failFirstReferrersCheck{Handler: inner, status: http.StatusForbidden})
	repo := testRepo(t)
	subject := pushSubject(t, w, repo)

	for _, marker := range []string{"one", "two"} {
		referrer := newReferrer(t, subject, marker)
		digest, _ := referrer.Digest()
		if err := w.Push(context.Background(), repo.Digest(digest.String()), referrer); err != nil {
			t.Fatalf("Push(%s) = %v", marker, err)
		}
	}
	if inner.tagWrites != 1 {
		t.Errorf("wrote the referrers tag %d times, want once (for the first referrer only)", inner.tagWrites)
	}
}
