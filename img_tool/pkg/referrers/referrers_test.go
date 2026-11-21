package referrers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// handlerTransport serves requests in-process: binding sockets is not possible
// in every environment the tests run in.
type handlerTransport struct{ handler http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, req)
	resp := recorder.Result()
	resp.Request = req
	return resp, nil
}

type rawManifest struct {
	raw       []byte
	mediaType types.MediaType
}

func (m rawManifest) RawManifest() ([]byte, error)       { return m.raw, nil }
func (m rawManifest) MediaType() (types.MediaType, error) { return m.mediaType, nil }

func put(t *testing.T, ref name.Reference, manifest map[string]any, options []remote.Option) v1.Hash {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(ref, rawManifest{raw: raw, mediaType: types.OCIManifestSchema1}, options...); err != nil {
		t.Fatalf("putting manifest: %v", err)
	}
	h, _, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// artifact returns a referrer manifest. The artifact type is also the config's
// media type (as older ORAS clients write it), since the in-memory registry's
// referrers API reports that instead of the manifest's artifactType.
func artifact(artifactType string, subject v1.Descriptor, marker string) map[string]any {
	empty := map[string]any{
		"mediaType": "application/vnd.oci.empty.v1+json",
		"digest":    "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
		"size":      2,
	}
	config := map[string]any{
		"mediaType": artifactType,
		"digest":    empty["digest"],
		"size":      2,
	}
	return map[string]any{
		"schemaVersion": 2,
		"mediaType":     string(types.OCIManifestSchema1),
		"artifactType":  artifactType,
		"config":        config,
		"layers":        []any{empty},
		"subject": map[string]any{
			"mediaType": string(subject.MediaType),
			"digest":    subject.Digest.String(),
			"size":      subject.Size,
		},
		"annotations": map[string]string{"marker": marker},
	}
}

func TestDiscover(t *testing.T) {
	for _, referrersAPI := range []bool{true, false} {
		name := "tag schema fallback"
		if referrersAPI {
			name = "referrers API"
		}
		t.Run(name, func(t *testing.T) {
			rt := handlerTransport{registry.New(registry.WithReferrersSupport(referrersAPI))}
			options := []remote.Option{remote.WithTransport(rt)}
			repo, err := nameRepository("registry.test/example/image")
			if err != nil {
				t.Fatal(err)
			}

			subjectManifest := map[string]any{
				"schemaVersion": 2,
				"mediaType":     string(types.OCIManifestSchema1),
				"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111", "size": 1},
				"layers":        []any{},
			}
			raw, _ := json.Marshal(subjectManifest)
			subjectDigest := put(t, repo.Tag("latest"), subjectManifest, options)
			subject := v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: subjectDigest, Size: int64(len(raw))}

			sig1 := put(t, repo.Tag("sig1"), artifact("application/vnd.cncf.notary.signature", subject, "1"), options)
			sig2 := put(t, repo.Tag("sig2"), artifact("application/vnd.cncf.notary.signature", subject, "2"), options)
			sbom := put(t, repo.Tag("sbom"), artifact("application/spdx+json", subject, "3"), options)

			found, err := Discover(context.Background(), repo, subjectDigest.String(), []string{
				"application/vnd.cncf.notary.signature",
				"application/vnd.dev.sigstore.bundle.v0.3+json",
			}, Options{Transport: rt, Remote: options})
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			wantSigs := []string{sig1.String(), sig2.String()}
			slices.Sort(wantSigs)
			if got := found["application/vnd.cncf.notary.signature"]; !slices.Equal(got, wantSigs) {
				t.Errorf("signatures = %v, want %v", got, wantSigs)
			}
			// A requested type without referrers is recorded as an empty list,
			// and an unrequested type (the SBOM) is not reported at all.
			if got, ok := found["application/vnd.dev.sigstore.bundle.v0.3+json"]; !ok || len(got) != 0 {
				t.Errorf("sigstore bundles = %v (present: %v), want an empty list", got, ok)
			}
			if _, ok := found["application/spdx+json"]; ok {
				t.Errorf("unrequested artifact type reported: %v", found)
			}
			if got := Flatten(found); !slices.Equal(got, wantSigs) {
				t.Errorf("Flatten = %v, want %v", got, wantSigs)
			}

			// "*" finds every referrer, next to the requested types it matches.
			all, err := Discover(context.Background(), repo, subjectDigest.String(), []string{
				AnyArtifactType,
				"application/vnd.cncf.notary.signature",
			}, Options{Transport: rt, Remote: options})
			if err != nil {
				t.Fatalf("Discover(*): %v", err)
			}
			wantAll := []string{sig1.String(), sig2.String(), sbom.String()}
			slices.Sort(wantAll)
			if got := all[AnyArtifactType]; !slices.Equal(got, wantAll) {
				t.Errorf("* = %v, want %v", got, wantAll)
			}
			if got := all["application/vnd.cncf.notary.signature"]; !slices.Equal(got, wantSigs) {
				t.Errorf("signatures next to * = %v, want %v", got, wantSigs)
			}
			if _, ok := all["application/spdx+json"]; ok {
				t.Errorf("unrequested artifact type reported next to *: %v", all)
			}

			// A manifest without referrers (or fallback tag) has none.
			none, err := Discover(context.Background(), repo, "sha256:2222222222222222222222222222222222222222222222222222222222222222", []string{"application/vnd.cncf.notary.signature"}, Options{Transport: rt, Remote: options})
			if err != nil {
				t.Fatalf("Discover without referrers: %v", err)
			}
			if len(Flatten(none)) != 0 {
				t.Errorf("got referrers %v for a manifest without any", none)
			}
		})
	}
}

func nameRepository(s string) (name.Repository, error) { return name.NewRepository(s) }

// A tag schema index is just a tag: it may list manifests without an artifact
// type, or manifests that are not referrers at all (a tag that happens to be
// named after the digest, as for a multi-platform image).
func TestDiscoverVerifiesTagSchemaEntries(t *testing.T) {
	rt := handlerTransport{registry.New(registry.WithReferrersSupport(false))}
	options := []remote.Option{remote.WithTransport(rt)}
	repo, err := nameRepository("registry.test/example/image")
	if err != nil {
		t.Fatal(err)
	}
	subjectManifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     string(types.OCIManifestSchema1),
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111", "size": 1},
		"layers":        []any{},
	}
	raw, _ := json.Marshal(subjectManifest)
	subjectDigest := put(t, repo.Tag("latest"), subjectManifest, options)
	subject := v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: subjectDigest, Size: int64(len(raw))}

	signature := artifact("application/vnd.dev.sigstore.bundle.v0.3+json", subject, "1")
	signatureDigest := put(t, repo.Tag("signature"), signature, options)
	other := v1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: v1.Hash{Algorithm: "sha256", Hex: "2222222222222222222222222222222222222222222222222222222222222222"}, Size: 1}
	strayDigest := put(t, repo.Tag("stray"), artifact("application/vnd.dev.sigstore.bundle.v0.3+json", other, "2"), options)
	plainDigest := put(t, repo.Tag("plain"), subjectManifest, options)
	mislabeledDigest := put(t, repo.Tag("mislabeled"), artifact("application/vnd.dev.sigstore.bundle.v0.3+json", subject, "3"), options)

	indexReferrer := map[string]any{
		"schemaVersion": 2,
		"mediaType":     string(types.OCIImageIndex),
		"artifactType":  "application/vnd.dev.sigstore.bundle.v0.3+json",
		"manifests":     []any{map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": plainDigest.String(), "size": len(raw)}},
		"subject":       map[string]any{"mediaType": string(subject.MediaType), "digest": subjectDigest.String(), "size": subject.Size},
	}
	indexReferrerRaw, _ := json.Marshal(indexReferrer)
	if err := remote.Put(repo.Tag("index-referrer"), rawManifest{raw: indexReferrerRaw, mediaType: types.OCIImageIndex}, options...); err != nil {
		t.Fatal(err)
	}
	indexReferrerDigest, _, _ := v1.SHA256(bytes.NewReader(indexReferrerRaw))

	// Overwrite the fallback tag that the pushes maintained.
	index := map[string]any{
		"schemaVersion": 2,
		"mediaType":     string(types.OCIImageIndex),
		"manifests": []any{
			map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": signatureDigest.String(), "size": 1},
			// cosign records the config's media type as the artifact type.
			map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": mislabeledDigest.String(), "size": 1, "artifactType": "application/vnd.oci.empty.v1+json"},
			map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": strayDigest.String(), "size": 1, "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json"},
			map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": plainDigest.String(), "size": 1},
			// An index (an ORAS index artifact, say) isn't an image manifest:
			// it's skipped, although it refers to the subject.
			map[string]any{"mediaType": string(types.OCIImageIndex), "digest": indexReferrerDigest.String(), "size": len(indexReferrerRaw), "artifactType": "application/vnd.dev.sigstore.bundle.v0.3+json"},
		},
	}
	indexRaw, _ := json.Marshal(index)
	if err := remote.Put(repo.Tag("sha256-"+subjectDigest.Hex), rawManifest{raw: indexRaw, mediaType: types.OCIImageIndex}, options...); err != nil {
		t.Fatal(err)
	}

	found, err := Discover(context.Background(), repo, subjectDigest.String(), []string{"application/vnd.dev.sigstore.bundle.v0.3+json"}, Options{Transport: rt, Remote: options})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// The untyped and the mislabeled signature are found through their own
	// manifests; the entry whose subject is another manifest, and the plain
	// image, are dropped.
	want := []string{signatureDigest.String(), mislabeledDigest.String()}
	slices.Sort(want)
	if got := found["application/vnd.dev.sigstore.bundle.v0.3+json"]; !slices.Equal(got, want) {
		t.Errorf("found %v, want %v", got, want)
	}

	// "*" doesn't trust the tag either.
	all, err := Discover(context.Background(), repo, subjectDigest.String(), []string{AnyArtifactType}, Options{Transport: rt, Remote: options})
	if err != nil {
		t.Fatalf("Discover(*): %v", err)
	}
	if got := all[AnyArtifactType]; !slices.Equal(got, want) {
		t.Errorf("* found %v, want %v", got, want)
	}
}

func TestTagSchemaTag(t *testing.T) {
	for _, tc := range []struct {
		digest string
		want   string
	}{
		{
			digest: "sha256:6534c5cbddc98d7c9f9a504b7de010cee8509f7afbe7717330b94a051925e6dd",
			want:   "sha256-6534c5cbddc98d7c9f9a504b7de010cee8509f7afbe7717330b94a051925e6dd",
		},
		{
			// The encoded section is truncated to 64 characters.
			digest: "sha512:" + strings.Repeat("0123456789abcdef", 8),
			want:   "sha512-" + strings.Repeat("0123456789abcdef", 4),
		},
		{
			// The algorithm is truncated to 32 characters, and characters a tag
			// can't contain become "-".
			digest: "multihash+base58:" + strings.Repeat("a", 40) + ":" + "b+c",
			want:   "multihash-base58-" + strings.Repeat("a", 40) + "-b-c",
		},
		{
			digest: strings.Repeat("x", 40) + ":abc",
			want:   strings.Repeat("x", 32) + "-abc",
		},
	} {
		got, err := TagSchemaTag(tc.digest)
		if err != nil {
			t.Errorf("TagSchemaTag(%q): %v", tc.digest, err)
			continue
		}
		if got != tc.want {
			t.Errorf("TagSchemaTag(%q) = %q, want %q", tc.digest, got, tc.want)
		}
	}
	if _, err := TagSchemaTag("nocolon"); err == nil {
		t.Errorf("TagSchemaTag accepted a digest without an algorithm")
	}
}

func TestNextLink(t *testing.T) {
	current, _ := url.Parse("https://registry.test/v2/example/image/referrers/sha256:abc?artifactType=a%2Fb")
	for _, tc := range []struct {
		headers []string
		want    string
	}{
		{headers: nil, want: ""},
		{headers: []string{`</v2/example/image/referrers/sha256:abc?n=10&last=x>; rel="next"`}, want: "https://registry.test/v2/example/image/referrers/sha256:abc?n=10&last=x"},
		{headers: []string{`<https://other.test/page2>; rel="prev", <https://other.test/page3>; rel="next"`}, want: "https://other.test/page3"},
		{headers: []string{`<page2?a=1,2>; rel=next`}, want: "https://registry.test/v2/example/image/referrers/page2?a=1,2"},
		{headers: []string{`</v2/x>; rel="first"`}, want: ""},
	} {
		got, err := nextLink(current, tc.headers)
		if err != nil {
			t.Errorf("nextLink(%v): %v", tc.headers, err)
			continue
		}
		gotStr := ""
		if got != nil {
			gotStr = got.String()
		}
		if gotStr != tc.want {
			t.Errorf("nextLink(%v) = %q, want %q", tc.headers, gotStr, tc.want)
		}
	}
}

// fakeReferrersRegistry serves the referrers API from canned responses, and
// everything else (the referrers tag, manifests) from an in-memory registry.
type fakeReferrersRegistry struct {
	backend   http.Handler
	responses map[string]func(w http.ResponseWriter, r *http.Request)
	requests  []string
}

func (f *fakeReferrersRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/referrers/") {
		f.requests = append(f.requests, r.URL.RequestURI())
		if respond, ok := f.responses[r.URL.RequestURI()]; ok {
			respond(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.backend.ServeHTTP(w, r)
}

func indexResponse(link string, digests ...string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		manifests := []map[string]any{}
		for _, d := range digests {
			manifests = append(manifests, map[string]any{"mediaType": string(types.OCIManifestSchema1), "digest": d, "size": 1, "artifactType": "application/vnd.cncf.notary.signature"})
		}
		if link != "" {
			w.Header().Set("Link", link)
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 2, "mediaType": string(types.OCIImageIndex), "manifests": manifests})
	}
}

func digestN(n int) string { return fmt.Sprintf("sha256:%064x", n) }

func TestListFollowsSpec(t *testing.T) {
	subject := digestN(1)
	base := "/v2/example/image/referrers/" + subject
	repo, err := nameRepository("registry.test/example/image")
	if err != nil {
		t.Fatal(err)
	}
	list := func(fake *fakeReferrersRegistry, artifactTypes ...string) ([]v1.Descriptor, Source, error) {
		rt := handlerTransport{fake}
		return List(context.Background(), repo, subject, artifactTypes, Options{Transport: rt, Remote: []remote.Option{remote.WithTransport(rt)}})
	}
	digestsOf := func(descriptors []v1.Descriptor) []string {
		var out []string
		for _, d := range descriptors {
			out = append(out, d.Digest.String())
		}
		return out
	}

	t.Run("follows Link pagination and passes a single artifactType filter", func(t *testing.T) {
		fake := &fakeReferrersRegistry{backend: registry.New(), responses: map[string]func(http.ResponseWriter, *http.Request){
			base + "?artifactType=application%2Fvnd.cncf.notary.signature": indexResponse(`<`+base+`?page=2>; rel="next"`, digestN(2)),
			base + "?page=2": indexResponse("", digestN(3)),
		}}
		descriptors, source, err := list(fake, "application/vnd.cncf.notary.signature")
		if err != nil {
			t.Fatal(err)
		}
		if source != SourceAPI || !slices.Equal(digestsOf(descriptors), []string{digestN(2), digestN(3)}) {
			t.Errorf("got %v from %s, want both pages from the API", digestsOf(descriptors), source)
		}
	})

	t.Run("falls back to the tag schema on 404 only", func(t *testing.T) {
		backend := registry.New()
		fake := &fakeReferrersRegistry{backend: backend}
		descriptors, source, err := list(fake, "a/b", "c/d")
		if err != nil || source != SourceTagSchema || len(descriptors) != 0 {
			t.Errorf("missing tag: got %v, %s, %v; want no referrers from the tag schema", descriptors, source, err)
		}
		// Two artifact types: no filter is passed.
		if !slices.Equal(fake.requests, []string{base}) {
			t.Errorf("requests = %v, want one unfiltered request", fake.requests)
		}

		for _, status := range []int{http.StatusBadRequest, http.StatusNotAcceptable, http.StatusUnauthorized, http.StatusInternalServerError} {
			fake := &fakeReferrersRegistry{backend: backend, responses: map[string]func(http.ResponseWriter, *http.Request){
				base: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) },
			}}
			if _, _, err := list(fake); err == nil {
				t.Errorf("status %d: got no error, want one (only 404 falls back)", status)
			}
		}
	})

	t.Run("rejects a 200 that is not an image index", func(t *testing.T) {
		fake := &fakeReferrersRegistry{backend: registry.New(), responses: map[string]func(http.ResponseWriter, *http.Request){
			base: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte("<html></html>"))
			},
		}}
		if _, _, err := list(fake); err == nil || !strings.Contains(err.Error(), "Content-Type") {
			t.Errorf("err = %v, want a Content-Type error", err)
		}
	})

	t.Run("a tag that is not an index means no referrers", func(t *testing.T) {
		backend := registry.New()
		rt := handlerTransport{backend}
		tag, _ := TagSchemaTag(subject)
		put(t, repo.Tag(tag), map[string]any{
			"schemaVersion": 2,
			"mediaType":     string(types.OCIManifestSchema1),
			"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": digestN(9), "size": 1},
			"layers":        []any{},
		}, []remote.Option{remote.WithTransport(rt)})
		descriptors, source, err := list(&fakeReferrersRegistry{backend: backend})
		if err != nil || source != SourceTagSchema || len(descriptors) != 0 {
			t.Errorf("got %v, %s, %v; want no referrers", descriptors, source, err)
		}
	})
}
