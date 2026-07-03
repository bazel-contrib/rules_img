package registry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManifestTTLEvictsTagDigestAndRepository(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	handler := New(
		WithManifestTTL(time.Minute),
		withManifestClock(func() time.Time { return now }),
	)

	digest := putManifest(t, handler, "repro/app", "v1", "manifest-one")
	assertManifestBody(t, handler, "repro/app", "v1", http.StatusOK, "manifest-one")
	assertManifestBody(t, handler, "repro/app", digest, http.StatusOK, "manifest-one")

	now = now.Add(time.Minute + time.Nanosecond)

	assertManifestBody(t, handler, "repro/app", "v1", http.StatusNotFound, "")
	assertManifestBody(t, handler, "repro/app", digest, http.StatusNotFound, "")
	assertStatus(t, handler, http.MethodGet, "/v2/repro/app/tags/list", http.StatusNotFound)
	assertBody(t, handler, http.MethodGet, "/v2/_catalog?n=1000", http.StatusOK, `{"repositories":null}`)
}

func TestManifestTTLDoesNotEvictRewrittenTagFromOlderExpiry(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	handler := New(
		WithManifestTTL(time.Minute),
		withManifestClock(func() time.Time { return now }),
	)

	oldDigest := putManifest(t, handler, "repro/app", "stable", "old-manifest")
	now = now.Add(30 * time.Second)
	newDigest := putManifest(t, handler, "repro/app", "stable", "new-manifest")

	now = now.Add(30*time.Second + time.Nanosecond)

	assertManifestBody(t, handler, "repro/app", "stable", http.StatusOK, "new-manifest")
	assertManifestBody(t, handler, "repro/app", oldDigest, http.StatusNotFound, "")
	assertManifestBody(t, handler, "repro/app", newDigest, http.StatusOK, "new-manifest")
	assertBody(t, handler, http.MethodGet, "/v2/repro/app/tags/list", http.StatusOK, `{"name":"repro/app","tags":["stable"]}`)
}

func TestManifestTTLDisabledPreservesManifest(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	handler := New(
		WithManifestTTL(0),
		withManifestClock(func() time.Time { return now }),
	)

	digest := putManifest(t, handler, "repro/app", "v1", "manifest-one")
	now = now.Add(365 * 24 * time.Hour)

	assertManifestBody(t, handler, "repro/app", "v1", http.StatusOK, "manifest-one")
	assertManifestBody(t, handler, "repro/app", digest, http.StatusOK, "manifest-one")
}

func TestManifestTTLUsesDefaultClock(t *testing.T) {
	handler := New(WithManifestTTL(time.Hour))

	digest := putManifest(t, handler, "repro/app", "v1", "manifest-one")

	assertManifestBody(t, handler, "repro/app", "v1", http.StatusOK, "manifest-one")
	assertManifestBody(t, handler, "repro/app", digest, http.StatusOK, "manifest-one")
}

func TestManifestTTLDeleteRemovesExpiryMetadata(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	handler := New(
		WithManifestTTL(time.Minute),
		withManifestClock(func() time.Time { return now }),
	)

	digest := putManifest(t, handler, "repro/app", "v1", "manifest-one")

	assertStatus(t, handler, http.MethodDelete, "/v2/repro/app/manifests/v1", http.StatusAccepted)
	now = now.Add(time.Minute + time.Nanosecond)

	assertManifestBody(t, handler, "repro/app", "v1", http.StatusNotFound, "")
	assertManifestBody(t, handler, "repro/app", digest, http.StatusNotFound, "")
}

func TestManifestTTLDisabledDeletePreservesCurrentDeleteBehavior(t *testing.T) {
	handler := New(WithManifestTTL(0))

	digest := putManifest(t, handler, "repro/app", "v1", "manifest-one")

	assertStatus(t, handler, http.MethodDelete, "/v2/repro/app/manifests/v1", http.StatusAccepted)
	assertManifestBody(t, handler, "repro/app", "v1", http.StatusNotFound, "")
	assertManifestBody(t, handler, "repro/app", digest, http.StatusOK, "manifest-one")
}

func putManifest(t *testing.T, handler http.Handler, repo, ref, body string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/"+repo+"/manifests/"+ref, strings.NewReader(body))

	handler.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("PUT manifest got status %d and body %q", recorder.Code, recorder.Body.String())
	}
	digest := recorder.Header().Get("Docker-Content-Digest")
	if digest == "" {
		t.Fatal("PUT manifest did not return Docker-Content-Digest")
	}
	return digest
}

func assertManifestBody(t *testing.T, handler http.Handler, repo, ref string, wantStatus int, wantBody string) {
	t.Helper()
	assertBody(t, handler, http.MethodGet, "/v2/"+repo+"/manifests/"+ref, wantStatus, wantBody)
}

func assertStatus(t *testing.T, handler http.Handler, method, path string, wantStatus int) {
	t.Helper()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)

	handler.ServeHTTP(recorder, req)

	if recorder.Code != wantStatus {
		t.Fatalf("%s %s got status %d and body %q, want %d", method, path, recorder.Code, recorder.Body.String(), wantStatus)
	}
}

func assertBody(t *testing.T, handler http.Handler, method, path string, wantStatus int, wantBody string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)

	handler.ServeHTTP(recorder, req)

	if recorder.Code != wantStatus {
		t.Fatalf("%s %s got status %d and body %q, want %d", method, path, recorder.Code, recorder.Body.String(), wantStatus)
	}
	if wantStatus >= http.StatusBadRequest && wantBody == "" {
		return
	}
	body, err := io.ReadAll(recorder.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if string(body) != wantBody {
		t.Fatalf("%s %s got body %q, want %q", method, path, string(body), wantBody)
	}
}
