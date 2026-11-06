package pushcmd

import (
	"bytes"
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
	"github.com/bazel-contrib/rules_img/img_tool/pkg/casdirlayout"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/compactstream"
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

// TestResolveDescriptorFromBlob verifies that, when no metadata file is provided,
// resolveDescriptor derives the descriptor by hashing the blob file. This is how
// the config blob (which has no standalone descriptor file) is pushed at build
// time: sha256(config file) is exactly the digest the manifest's config descriptor
// references.
func TestResolveDescriptorFromBlob(t *testing.T) {
	dir := t.TempDir()
	blobPath, want := writeBlobFile(t, dir)

	got, err := resolveDescriptor("", blobPath, "application/vnd.oci.image.config.v1+json")
	if err != nil {
		t.Fatalf("resolveDescriptor: %v", err)
	}
	if got.Digest != want.Digest {
		t.Errorf("Digest = %q, want %q", got.Digest, want.Digest)
	}
	if got.Size != want.Size {
		t.Errorf("Size = %d, want %d", got.Size, want.Size)
	}
	if got.MediaType != "application/vnd.oci.image.config.v1+json" {
		t.Errorf("MediaType = %q, want the value passed in", got.MediaType)
	}
}

// TestResolveDescriptorRequiresSource verifies resolveDescriptor errors when it has
// neither a metadata file nor a blob to hash.
func TestResolveDescriptorRequiresSource(t *testing.T) {
	if _, err := resolveDescriptor("", "", ""); err == nil {
		t.Error("expected an error when neither --metadata nor --blob is provided")
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

// TestLayerForBlobReadsCASDirLayouts checks that a layer reconstructed from a
// compact stream finds its files in the content-addressed directory that
// `img cas-dir` writes (sharded by digest prefix), as well as in the flat layout
// of older versions.
func TestLayerForBlobReadsCASDirLayouts(t *testing.T) {
	content := []byte("a file large enough to be referenced from the compact stream by digest")
	sum := sha256.Sum256(content)
	hexDigest := hex.EncodeToString(sum[:])

	for _, tc := range []struct {
		name string
		path func(shaDir string) string
	}{
		{"sharded", func(shaDir string) string { return casdirlayout.Path(shaDir, hexDigest) }},
		{"flat", func(shaDir string) string { return filepath.Join(shaDir, hexDigest) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			casDir := t.TempDir()
			blobPath := tc.path(filepath.Join(casDir, "sha256"))
			if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(blobPath, content, 0o644); err != nil {
				t.Fatal(err)
			}

			var stream bytes.Buffer
			w := compactstream.NewWriter(&stream, compactstream.HashAlgoSHA256, 32, compactstream.StreamCompressionZstd,
				compactstream.OriginalCompressionInfo{Compression: compactstream.OriginalCompressionNone}, 0)
			if err := w.WriteStreamBytes([]byte("head-")); err != nil {
				t.Fatal(err)
			}
			if err := w.WriteCASRef(sum[:], uint64(len(content))); err != nil {
				t.Fatal(err)
			}
			if err := w.WriteStreamBytes([]byte("-tail")); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			streamPath := filepath.Join(t.TempDir(), "layer.cstream")
			if err := os.WriteFile(streamPath, stream.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}

			layer, err := layerForBlob(context.Background(), api.Descriptor{}, "", streamPath, casDir, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			r, err := layer.Compressed()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("reconstructing the layer: %v", err)
			}
			if want := "head-" + string(content) + "-tail"; string(got) != want {
				t.Fatalf("reconstructed layer = %q, want %q", got, want)
			}
		})
	}
}

// writeTestFile writes content to name in dir and returns its path.
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestReadConfigurationUsesTheConfigurationFile verifies that, without a
// destination file, the registry and repository come from the configuration.
func TestReadConfigurationUsesTheConfigurationFile(t *testing.T) {
	dir := t.TempDir()
	configPath := writeTestFile(t, dir, "config.json", `{"registry":"ghcr.io","repository":"acme/app"}`)

	cfg, err := readConfiguration(configPath, "")
	if err != nil {
		t.Fatalf("readConfiguration: %v", err)
	}
	if cfg.Registry != "ghcr.io" || cfg.Repository != "acme/app" {
		t.Errorf("got %+v, want ghcr.io/acme/app", cfg)
	}
}

// TestReadConfigurationPrefersTheDestinationFile verifies that a destination file
// supplies the registry and repository, so a push spec using destination_file
// (whose configuration has neither) pushes its blobs to the same repository as
// the deploy manifest.
func TestReadConfigurationPrefersTheDestinationFile(t *testing.T) {
	dir := t.TempDir()
	configPath := writeTestFile(t, dir, "config.json", `{"registry":"","repository":"","tags":["latest"]}`)
	destinationPath := writeTestFile(t, dir, "destination.txt", "ghcr.io/acme/app\n")

	cfg, err := readConfiguration(configPath, destinationPath)
	if err != nil {
		t.Fatalf("readConfiguration: %v", err)
	}
	if cfg.Registry != "ghcr.io" || cfg.Repository != "acme/app" {
		t.Errorf("got %+v, want ghcr.io/acme/app", cfg)
	}
}

// TestReadConfigurationRequiresADestination verifies that a configuration without
// registry/repository is rejected when there is no destination file either.
func TestReadConfigurationRequiresADestination(t *testing.T) {
	dir := t.TempDir()
	configPath := writeTestFile(t, dir, "config.json", `{"registry":"","repository":""}`)

	if _, err := readConfiguration(configPath, ""); err == nil {
		t.Error("readConfiguration succeeded, want an error")
	}
}
