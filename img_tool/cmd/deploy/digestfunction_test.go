package deploy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/blake3sum"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

func TestDigestFunctionSettingFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    string
		wantErr bool
	}{
		{"", "auto", false},
		{"auto", "auto", false},
		{"sha256", "sha256", false},
		{"blake3", "blake3", false},
		{" BLAKE3 ", "blake3", false},
		{"md5", "", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv(envDigestFunction, tc.value)
			got, err := digestFunctionSettingFromEnv()
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("digestFunctionSettingFromEnv() = %q, %v; want %q, error: %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestResolveDigestFunctions(t *testing.T) {
	supports := func(functions ...string) func(context.Context) ([]string, error) {
		return func(context.Context) ([]string, error) { return functions, nil }
	}
	failing := func(err error) func(context.Context) ([]string, error) {
		return func(context.Context) ([]string, error) { return nil, err }
	}
	neverAsked := func(context.Context) ([]string, error) {
		t.Error("an explicit setting asked the cache for its capabilities")
		return nil, nil
	}

	for _, tc := range []struct {
		name        string
		setting     string
		supported   func(context.Context) ([]string, error)
		want        []string
		wantWarning bool
	}{
		{"explicit sha256", "sha256", neverAsked, []string{"sha256"}, false},
		{"explicit blake3", "blake3", neverAsked, []string{"blake3"}, false},
		{"auto, sha256 cache", "auto", supports("sha256"), []string{"sha256"}, false},
		{"auto, blake3-only cache", "auto", supports("blake3"), []string{"blake3"}, false},
		{"auto, cache with both tries sha256 first", "auto", supports("sha256", "blake3"), []string{"sha256", "blake3"}, false},
		{"auto, cache with only unusable functions", "auto", supports("md5"), []string{"sha256"}, false},
		{"auto, cache that predates capabilities", "auto", failing(status.Error(codes.Unimplemented, "no")), []string{"sha256"}, false},
		{"auto, cache that does not answer", "auto", failing(errors.New("boom")), []string{"sha256"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var warnings bytes.Buffer
			got := resolveDigestFunctions(context.Background(), tc.setting, tc.supported, &warnings)
			if !slices.Equal(got, tc.want) {
				t.Errorf("resolveDigestFunctions = %v, want %v", got, tc.want)
			}
			if (warnings.Len() > 0) != tc.wantWarning {
				t.Errorf("warnings = %q, want a warning: %v", warnings.String(), tc.wantWarning)
			}
		})
	}
}

func TestApplyToolSettingsDigestFunction(t *testing.T) {
	t.Run("fills an empty environment", func(t *testing.T) {
		t.Setenv(envDigestFunction, "")
		applyToolSettings(&api.ToolSettings{DigestFunction: "blake3"}, io.Discard)
		if got := os.Getenv(envDigestFunction); got != "blake3" {
			t.Errorf("%s = %q, want blake3", envDigestFunction, got)
		}
	})
	t.Run("the environment wins", func(t *testing.T) {
		t.Setenv(envDigestFunction, "sha256")
		applyToolSettings(&api.ToolSettings{DigestFunction: "blake3"}, io.Discard)
		if got := os.Getenv(envDigestFunction); got != "sha256" {
			t.Errorf("%s = %q, want the environment's sha256", envDigestFunction, got)
		}
	})
}

// digestKeyedSource is a remote cache that holds each blob under the one digest
// function it was stored with, as a Bazel cache does, and records every read.
type digestKeyedSource struct {
	mu    sync.Mutex
	blobs map[string][]byte // "algorithm:hex" -> content
	reads []string
}

func (s *digestKeyedSource) FindMissingBlobs(_ context.Context, digests []cas.Digest) ([]cas.Digest, error) {
	var missing []cas.Digest
	for _, digest := range digests {
		if _, found := s.blobs[digestKey(digest)]; !found {
			missing = append(missing, digest)
		}
	}
	return missing, nil
}

func (s *digestKeyedSource) ReadBlob(_ context.Context, digest cas.Digest) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, found := s.blobs[digestKey(digest)]
	if !found {
		return nil, fmt.Errorf("blob %s not in the fake CAS", digestKey(digest))
	}
	s.reads = append(s.reads, digestKey(digest))
	return content, nil
}

func (s *digestKeyedSource) ReaderForBlob(ctx context.Context, digest cas.Digest) (io.ReadCloser, error) {
	content, err := s.ReadBlob(ctx, digest)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (s *digestKeyedSource) ReaderForBlobs(ctx context.Context, digests []cas.Digest) (io.ReadCloser, error) {
	var buf bytes.Buffer
	for _, digest := range digests {
		content, err := s.ReadBlob(ctx, digest)
		if err != nil {
			return nil, err
		}
		buf.Write(content)
	}
	return io.NopCloser(&buf), nil
}

func digestKey(digest cas.Digest) string {
	return digest.Algorithm() + ":" + hex.EncodeToString(digest.Hash)
}

func blake3Hex(t *testing.T, content []byte) string {
	t.Helper()
	sum, _, err := blake3sum.Sum(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sum)
}

// TestLazyPushAgainstDigestKeyedCache pushes images whose layers a remote cache
// holds under SHA256 (an ordinary Bazel) or under BLAKE3 (--digest_function=blake3).
// Either way the registry receives the layers under their SHA256 digests, and the
// cache is only ever asked for them by the digest function it stores them by.
func TestLazyPushAgainstDigestKeyedCache(t *testing.T) {
	for _, tc := range []struct {
		name      string
		blake3    bool
		functions []string
	}{
		{"sha256 cache", false, []string{"sha256"}},
		{"blake3 cache, auto-detected", true, []string{"sha256", "blake3"}},
		{"blake3 cache, layers recorded but sha256 configured", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOCKER_CONFIG", t.TempDir()) // no credential helpers of the machine running the test
			_, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 2, 1)
			layers := append(slices.Clone(images.shared), images.unique...)

			// The layers' BLAKE3 digests, which their builds recorded in the deploy manifest.
			for i, raw := range dm.Operations {
				var op api.PushDeployOperation
				if err := json.Unmarshal(raw, &op); err != nil {
					t.Fatal(err)
				}
				for j := range op.Manifests[0].LayerBlobs {
					layer := &op.Manifests[0].LayerBlobs[j]
					_, hexDigest, _ := strings.Cut(layer.Digest, ":")
					layer.CASDigests = map[string]string{api.DigestBLAKE3: blake3Hex(t, images.blobs[hexDigest])}
				}
				encoded, err := json.Marshal(op)
				if err != nil {
					t.Fatal(err)
				}
				dm.Operations[i] = encoded
			}

			source := &digestKeyedSource{blobs: map[string][]byte{}}
			wantRead := map[string]bool{}
			for hexDigest, content := range images.blobs {
				source.blobs["sha256:"+hexDigest] = content
			}
			for _, layer := range layers {
				_, hexDigest, _ := strings.Cut(layer, ":")
				if tc.blake3 {
					// A BLAKE3 cache holds the layer under its BLAKE3 digest, and not under its SHA256 digest.
					delete(source.blobs, "sha256:"+hexDigest)
					key := "blake3:" + blake3Hex(t, images.blobs[hexDigest])
					source.blobs[key] = images.blobs[hexDigest]
					wantRead[key] = true
				} else {
					wantRead["sha256:"+hexDigest] = true
				}
			}

			cache, err := cas.NewCachingReader(source, cas.WithCacheDir(t.TempDir()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cache.Close() })
			builder := deployvfs.NewBuilder(dm).WithCASReader(cache)
			if tc.functions != nil {
				builder = builder.WithCASDigestFunctions(tc.functions...)
			}
			// Layers that only a BLAKE3 cache holds cannot be found by SHA256 alone,
			// and the error has to say what to change.
			unreachable := tc.blake3 && tc.functions == nil
			vfs, err := builder.Build()
			if unreachable {
				if err == nil || !strings.Contains(err.Error(), "IMG_DIGEST_FUNCTION=blake3") {
					t.Fatalf("building the VFS over a BLAKE3 cache with only SHA256 configured: error = %v, want one pointing at the digest function setting", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

			reg := newNaiveRegistry()
			pushOps, err := dm.PushOperations()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := push.NewBuilder(vfs).
				WithJobs(2).
				WithRemoteOptions(registryopts.Default().WithTransport(reg.transport()).Remote()...).
				Build().
				PushAll(context.Background(), pushOps, dm.Settings.PushStrategy); err != nil {
				t.Fatalf("push: %v", err)
			}

			for _, layer := range layers {
				if got := reg.countBlobPuts(layer); got == 0 {
					t.Errorf("layer %s never reached the registry under its SHA256 digest", layer)
				}
			}
			for key := range wantRead {
				if !slices.Contains(source.reads, key) {
					t.Errorf("layer %s was never read from the cache", key)
				}
			}
		})
	}
}
