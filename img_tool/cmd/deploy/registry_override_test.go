package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/persistentworker"
)

// Exercise both entry points against a registry that requires source pull scope
// for mounts, without challenging for the missing scope on a failed mount.
func TestDeployRegistryOverrideCrossMount(t *testing.T) {
	const public = "registry.example.com"
	const private = "registry-internal.example.com"
	for _, worker := range []bool{false, true} {
		for _, tc := range []struct {
			name, override, hintRegistry string
			separateRegistry, mixedDedup bool
			wantReads, wantMounts        int
		}{
			{name: "same host", hintRegistry: public, wantMounts: 1},
			{name: "alias override", override: private, hintRegistry: public, wantMounts: 1},
			{name: "mixed dedup", override: private, hintRegistry: public, mixedDedup: true, wantMounts: 1},
			{name: "unrelated hint", override: private, hintRegistry: "other.example.com", wantReads: 1},
			{name: "different registry fallback", override: private, hintRegistry: public, separateRegistry: true, wantReads: 1},
		} {
			t.Run(fmt.Sprintf("worker=%t/%s", worker, tc.name), func(t *testing.T) {
				for _, key := range []string{"IMG_REGISTRY_GATEWAY", "IMG_REGISTRY_PUSH_GATEWAY", "IMG_REGISTRY_PULL_GATEWAY", "IMG_REAPI_ENDPOINT", "IMG_DISK_CACHE", "IMG_CAS_CACHE"} {
					t.Setenv(key, "")
				}
				layoutDirs, dm, images := buildSharedLayerLayouts(t, public, 2, 0)
				baseDigest := images.unique[0]
				const baseRepository = "team/base"
				for i, raw := range dm.Operations {
					var op api.PushDeployOperation
					if err := json.Unmarshal(raw, &op); err != nil {
						t.Fatal(err)
					}
					op.DeduplicatedPush = api.DeduplicatedPushDisabled
					if i == 0 {
						op.CrossMountHint = &api.CrossMountSource{Registry: tc.hintRegistry, Repository: baseRepository}
						op.Manifests[0].LayerBlobs[0].Sources = []api.LayerSource{{Registry: public, Repository: baseRepository}}
					} else if tc.mixedDedup {
						op.DeduplicatedPush = api.DeduplicatedPushEnabled
					}
					var err error
					dm.Operations[i], err = json.Marshal(op)
					if err != nil {
						t.Fatal(err)
					}
				}
				// The base blob is remote-only: a refused mount must download it from
				// the original public host, even if --registry names a different one.
				if err := os.Remove(filepath.Join(layoutDirs[0], "blobs", "sha256", strings.TrimPrefix(baseDigest, "sha256:"))); err != nil {
					t.Fatal(err)
				}
				source := newNaiveRegistry()
				source.putBlob(baseRepository, baseDigest, images.blobs[strings.TrimPrefix(baseDigest, "sha256:")])
				destination := source
				if tc.separateRegistry {
					destination = newNaiveRegistry()
				}
				var sourceReads atomic.Int32
				var mu sync.Mutex
				tokens := map[string]string{}
				transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
					w := httptest.NewRecorder()
					if req.URL.Path == "/token" {
						mu.Lock()
						token := fmt.Sprintf("token-%d", len(tokens))
						tokens[token] = strings.Join(req.URL.Query()["scope"], " ")
						mu.Unlock()
						w.Header().Set("Content-Type", "application/json")
						json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_in": 3600})
					} else if req.URL.Path == "/v2/" || req.URL.Path == "/v2" {
						w.Header().Set("WWW-Authenticate", `Bearer realm="https://`+req.URL.Host+`/token",service="registry"`)
						w.WriteHeader(http.StatusUnauthorized)
					} else {
						from := req.URL.Query().Get("from")
						mu.Lock()
						scopes := tokens[strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")]
						mu.Unlock()
						if req.Method == http.MethodPost && from != "" && !slices.Contains(strings.Fields(scopes), "repository:"+from+":pull") {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusUnauthorized)
							io.WriteString(w, `{"errors":[{"code":"UNAUTHORIZED","message":"source pull scope missing"}]}`)
						} else {
							if req.Method == http.MethodGet && req.URL.Host == public && req.URL.Path == "/v2/"+baseRepository+"/blobs/"+baseDigest {
								sourceReads.Add(1)
							}
							switch req.URL.Host {
							case public:
								return source.transport().RoundTrip(req)
							case private:
								return destination.transport().RoundTrip(req)
							default:
								return nil, fmt.Errorf("unexpected registry %s", req.URL.Host)
							}
						}
					}
					resp := w.Result()
					resp.Request = req
					return resp, nil
				})
				originalTransport := remote.DefaultTransport
				remote.DefaultTransport = transport
				t.Cleanup(func() { remote.DefaultTransport = originalTransport })
				rawRequest, err := json.Marshal(dm)
				if err != nil {
					t.Fatal(err)
				}
				if worker {
					requestFile := filepath.Join(t.TempDir(), "deploy.json")
					if err := os.WriteFile(requestFile, rawRequest, 0600); err != nil {
						t.Fatal(err)
					}
					h, err := newDeployWorkerHandler(context.Background(), 1, "", dedupFlags{}, "none")
					if err != nil {
						t.Fatal(err)
					}
					defer h.Close()
					args := []string{"--request-file=" + requestFile}
					if tc.override != "" {
						args = append(args, "--registry="+tc.override)
					}
					for _, dir := range layoutDirs {
						args = append(args, "--oci-layout="+dir)
					}
					response := h.HandleRequest(context.Background(), persistentworker.WorkRequest{Arguments: args})
					if response.ExitCode != 0 {
						t.Fatalf("worker: %s", response.Output)
					}
				} else {
					if err := DeployWithExtras(context.Background(), rawRequest, DeployOptions{Jobs: 1, OverrideRegistry: tc.override, OCILayouts: layoutDirs}); err != nil {
						t.Fatal(err)
					}
				}
				if got := int(sourceReads.Load()); got != tc.wantReads {
					t.Errorf("base downloads = %d, want %d", got, tc.wantReads)
				}
				if got := destination.countBlobPuts(baseDigest); got != tc.wantReads {
					t.Errorf("base uploads = %d, want %d", got, tc.wantReads)
				}
				if got := len(destination.mounts); got != tc.wantMounts {
					t.Errorf("mounts = %d, want %d", got, tc.wantMounts)
				}
				if !destination.hasBlob(images.repositories[0], baseDigest) {
					t.Error("destination is missing the base blob")
				}
				destination.assertMountsCarryNoOrigin(t)
			})
		}
	}
}
