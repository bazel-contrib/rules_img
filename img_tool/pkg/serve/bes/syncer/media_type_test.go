package syncer

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
)

// mapCAS serves blobs by sha256 hex digest.
type mapCAS map[string][]byte

func (m mapCAS) blob(d cas.Digest) ([]byte, error) {
	data, ok := m[hex.EncodeToString(d.Hash)]
	if !ok {
		return nil, fmt.Errorf("blob %x not in CAS", d.Hash)
	}
	return data, nil
}

func (m mapCAS) ReadBlob(_ context.Context, d cas.Digest) ([]byte, error) { return m.blob(d) }

func (m mapCAS) ReaderForBlob(_ context.Context, d cas.Digest) (io.ReadCloser, error) {
	data, err := m.blob(d)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m mapCAS) ReaderForBlobs(_ context.Context, ds []cas.Digest) (io.ReadCloser, error) {
	var buf bytes.Buffer
	for _, d := range ds {
		data, err := m.blob(d)
		if err != nil {
			return nil, err
		}
		buf.Write(data)
	}
	return io.NopCloser(&buf), nil
}

func (m mapCAS) add(data []byte) v1.Hash {
	h, _, _ := v1.SHA256(bytes.NewReader(data))
	m[h.Hex] = data
	return h
}

// manifestContentTypes records the Content-Type of every manifest PUT.
type manifestContentTypes struct {
	mu   sync.Mutex
	puts map[string]string // path -> Content-Type
}

func (r *manifestContentTypes) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/manifests/") {
			r.mu.Lock()
			r.puts[req.URL.Path] = req.Header.Get("Content-Type")
			r.mu.Unlock()
		}
		next.ServeHTTP(w, req)
	})
}

// TestPushManifestWithoutMediaType pushes a manifest that omits its top-level
// mediaType (image_manifest's omit_media_type), alone and as the child of an
// index. The registry must still receive the image manifest Content-Type.
func TestPushManifestWithoutMediaType(t *testing.T) {
	store := mapCAS{}
	cfg := []byte(`{"name":"chart","version":"1.0.0"}`)
	layer := []byte("chart-content")
	cfgHash := store.add(cfg)
	layerHash := store.add(layer)
	manifest, err := json.Marshal(v1.Manifest{
		SchemaVersion: 2,
		Config:        v1.Descriptor{MediaType: "application/vnd.cncf.helm.config.v1+json", Digest: cfgHash, Size: int64(len(cfg))},
		Layers:        []v1.Descriptor{{MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip", Digest: layerHash, Size: int64(len(layer))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(manifest, []byte(`"mediaType":"`+api.MediaTypeImageManifest+`"`)) {
		t.Fatalf("test manifest has a top-level mediaType: %s", manifest)
	}
	manifestHash := store.add(manifest)
	manifestDesc := api.Descriptor{MediaType: api.MediaTypeImageManifest, Digest: manifestHash.String(), Size: int64(len(manifest))}
	index, err := json.Marshal(v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		Manifests:     []v1.Descriptor{{MediaType: types.OCIManifestSchema1, Digest: manifestHash, Size: int64(len(manifest))}},
	})
	if err != nil {
		t.Fatal(err)
	}
	indexHash := store.add(index)
	manifestInfo := api.ManifestDeployInfo{
		Descriptor: manifestDesc,
		Config:     api.Descriptor{MediaType: "application/vnd.cncf.helm.config.v1+json", Digest: cfgHash.String(), Size: int64(len(cfg))},
		LayerBlobs: []api.LayerBlob{{Descriptor: api.Descriptor{MediaType: "application/vnd.cncf.helm.chart.content.v1.tar+gzip", Digest: layerHash.String(), Size: int64(len(layer))}}},
	}

	tests := []struct {
		name string
		root api.Descriptor
		push func(*Syncer, context.Context, name.Repository, api.IndexedPushDeployOperation) error
	}{
		{
			name: "manifest",
			root: manifestDesc,
			push: func(s *Syncer, ctx context.Context, ref name.Repository, op api.IndexedPushDeployOperation) error {
				return s.pushImage(ctx, ref, op, nil)
			},
		},
		{
			name: "index child",
			root: api.Descriptor{MediaType: string(types.OCIImageIndex), Digest: indexHash.String(), Size: int64(len(index))},
			push: func(s *Syncer, ctx context.Context, ref name.Repository, op api.IndexedPushDeployOperation) error {
				return s.pushIndex(ctx, ref, op, nil)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &manifestContentTypes{puts: map[string]string{}}
			server := httptest.NewServer(recorder.wrap(registry.New(registry.Logger(log.New(io.Discard, "", 0)))))
			defer server.Close()
			ref, err := name.NewRepository(strings.TrimPrefix(server.URL, "http://") + "/charts/chart")
			if err != nil {
				t.Fatal(err)
			}

			s := NewWithWorkers(nil, 2)
			defer s.Shutdown()
			s.casClient = store
			op := api.IndexedPushDeployOperation{PushDeployOperation: api.PushDeployOperation{
				BaseCommandOperation: api.BaseCommandOperation{Command: "push", Root: tt.root, Manifests: []api.ManifestDeployInfo{manifestInfo}},
			}}
			if err := tt.push(s, context.Background(), ref, op); err != nil {
				t.Fatalf("push: %v", err)
			}

			path := "/v2/charts/chart/manifests/" + manifestHash.String()
			got, ok := recorder.puts[path]
			if !ok {
				t.Fatalf("no PUT to %s; PUTs: %v", path, recorder.puts)
			}
			if got != api.MediaTypeImageManifest {
				t.Errorf("manifest PUT Content-Type = %q, want %s", got, api.MediaTypeImageManifest)
			}
		})
	}
}
