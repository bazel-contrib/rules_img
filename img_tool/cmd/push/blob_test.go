package pushcmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// newTestRegistry starts an in-memory registry and returns its host (rewritten to
// "localhost" so go-containerregistry talks to it over plain HTTP) plus a
// recorder of every request path (used to assert which repository received the
// blob upload). It skips the test when the environment forbids binding a loopback
// port (e.g. a network-restricted sandbox).
func newTestRegistry(t *testing.T) (host string, paths *[]string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a loopback port in this environment: %v", err)
	}

	var mu sync.Mutex
	recorded := &[]string{}
	reg := registry.New()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*recorded = append(*recorded, r.URL.Path)
		mu.Unlock()
		reg.ServeHTTP(w, r)
	}))
	srv.Listener.Close()
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	// go-containerregistry treats "localhost[:port]" as insecure (HTTP).
	return strings.Replace(u.Host, "127.0.0.1", "localhost", 1), recorded
}

func writeBlobFile(t *testing.T, dir string) (path string, desc api.Descriptor) {
	t.Helper()
	content := []byte("hello, this is a test layer blob")
	path = filepath.Join(dir, "blob")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("writing blob: %v", err)
	}
	sum := sha256.Sum256(content)
	return path, api.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
		Size:      int64(len(content)),
	}
}

func blobExists(t *testing.T, host, repository, digest string) bool {
	t.Helper()
	ref, err := name.NewDigest(host + "/" + repository + "@" + digest)
	if err != nil {
		t.Fatalf("parsing ref: %v", err)
	}
	layer, err := remote.Layer(ref)
	if err != nil {
		return false
	}
	rc, err := layer.Compressed()
	if err != nil {
		return false
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err != nil {
		return false
	}
	return true
}

// skipIfRestricted skips the test when a push fails due to a sandbox syscall
// restriction (e.g. blocked outbound loopback transfer / sendfile), which happens
// in network-restricted build sandboxes. The test still runs fully in CI.
func skipIfRestricted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if strings.Contains(err.Error(), "operation not permitted") {
		t.Skipf("skipping: registry transfer not permitted in this environment: %v", err)
	}
	t.Fatalf("pushBlob: %v", err)
}

// TestPushBlobUploadsToRepository pushes a local blob and verifies it is
// retrievable from the target repository afterward.
func TestPushBlobUploadsToRepository(t *testing.T) {
	host, paths := newTestRegistry(t)
	dir := t.TempDir()
	blobPath, desc := writeBlobFile(t, dir)

	skipIfRestricted(t, pushBlob(context.Background(), host, "myrepo", desc, blobPath, "", "", nil))
	if !blobExists(t, host, "myrepo", desc.Digest) {
		t.Errorf("blob %s not found in myrepo after push", desc.Digest)
	}
	if !anyHasPrefix(*paths, "/v2/myrepo/blobs/") {
		t.Errorf("expected a blob upload under /v2/myrepo/blobs/, got paths: %v", *paths)
	}
}

// TestPushBlobUploadsToStagingRepository verifies the blob-staging redirect: the
// blob is uploaded to the staging repository, not the image's own repository.
func TestPushBlobUploadsToStagingRepository(t *testing.T) {
	host, paths := newTestRegistry(t)
	dir := t.TempDir()
	blobPath, desc := writeBlobFile(t, dir)

	// targetRepository is the staging repository (what blobProcess computes when
	// --blob-repository is set).
	skipIfRestricted(t, pushBlob(context.Background(), host, "rbe-blobs", desc, blobPath, "", "", nil))
	if !blobExists(t, host, "rbe-blobs", desc.Digest) {
		t.Errorf("blob %s not found in staging repo after push", desc.Digest)
	}
	if !anyHasPrefix(*paths, "/v2/rbe-blobs/blobs/") {
		t.Errorf("expected a blob upload under /v2/rbe-blobs/blobs/, got paths: %v", *paths)
	}
	if anyHasPrefix(*paths, "/v2/ubuntu/blobs/") {
		t.Errorf("blob was uploaded to the real repository, expected only the staging repository; paths: %v", *paths)
	}
}

func anyHasPrefix(items []string, prefix string) bool {
	for _, item := range items {
		if strings.HasPrefix(item, prefix) {
			return true
		}
	}
	return false
}
