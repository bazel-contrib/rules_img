package load

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// testVFS serves a single image (its manifest, config and one layer) out of
// memory, which is all streamDockerTar needs.
type testVFS struct {
	image        registryv1.Image
	manifestHash registryv1.Hash
	blobs        map[registryv1.Hash]registryv1.Layer
}

func newTestVFS(t *testing.T) *testVFS {
	t.Helper()

	img, err := mutate.AppendLayers(empty.Image, static.NewLayer([]byte("hello"), types.OCILayer))
	if err != nil {
		t.Fatalf("building test image: %v", err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	rawManifest, err := img.RawManifest()
	if err != nil {
		t.Fatalf("raw manifest: %v", err)
	}
	manifestHash, _, err := registryv1.SHA256(bytes.NewReader(rawManifest))
	if err != nil {
		t.Fatalf("hashing manifest: %v", err)
	}
	rawConfig, err := img.RawConfigFile()
	if err != nil {
		t.Fatalf("raw config: %v", err)
	}

	v := &testVFS{
		image:        img,
		manifestHash: manifestHash,
		blobs: map[registryv1.Hash]registryv1.Layer{
			manifestHash:              static.NewLayer(rawManifest, manifest.MediaType),
			manifest.Config.Digest:    static.NewLayer(rawConfig, manifest.Config.MediaType),
			manifest.Layers[0].Digest: static.NewLayer([]byte("hello"), types.OCILayer),
		},
	}
	return v
}

func (v *testVFS) ImageIndex(registryv1.Hash) (registryv1.ImageIndex, error) {
	return nil, fmt.Errorf("no index in this test VFS")
}

func (v *testVFS) Image(digest registryv1.Hash) (registryv1.Image, error) {
	if digest != v.manifestHash {
		return nil, fmt.Errorf("unknown image %s", digest)
	}
	return v.image, nil
}

func (v *testVFS) Layer(digest registryv1.Hash) (registryv1.Layer, error) {
	layer, ok := v.blobs[digest]
	if !ok {
		return nil, fmt.Errorf("unknown blob %s", digest)
	}
	return layer, nil
}

func (v *testVFS) ManifestBlob(digest registryv1.Hash) (registryv1.Layer, error) {
	return v.Layer(digest)
}

func (v *testVFS) DigestsFromRoot(registryv1.Hash) ([]registryv1.Hash, error) {
	return nil, fmt.Errorf("not needed in this test VFS")
}

func (v *testVFS) SizeOf(digest registryv1.Hash) (int64, error) {
	layer, err := v.Layer(digest)
	if err != nil {
		return 0, err
	}
	return layer.Size()
}

// repoTagsFromTar reads the RepoTags of the docker-save manifest.json inside a
// streamed tarball.
func repoTagsFromTar(t *testing.T, data []byte) []string {
	t.Helper()

	tr := tar.NewReader(bytes.NewReader(data))
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		if header.Name != "manifest.json" {
			continue
		}
		var entries []struct {
			RepoTags []string `json:"RepoTags"`
		}
		if err := json.NewDecoder(tr).Decode(&entries); err != nil {
			t.Fatalf("decoding manifest.json: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("got %d manifest.json entries, want 1", len(entries))
		}
		return entries[0].RepoTags
	}
	t.Fatal("no manifest.json in the streamed tarball")
	return nil
}

// TestStreamDockerTarUsesConfiguredName is the end-to-end regression guard for
// the reported bug: a registry with a port must reach the tarball intact, and
// nothing may prepend docker.io/library to the name.
func TestStreamDockerTarUsesConfiguredName(t *testing.T) {
	for _, tc := range []struct {
		name       string
		registry   string
		repository string
		tags       []string
		want       []string
	}{
		{
			name:       "registry with a port",
			registry:   "docker.mycompany.tld:1234",
			repository: "foo",
			tags:       []string{"latest"},
			want:       []string{"docker.mycompany.tld:1234/foo:latest"},
		},
		{
			name: "bare name is not expanded to docker.io/library",
			tags: []string{"funny_name:sideloaded"},
			want: []string{"funny_name:sideloaded"},
		},
		{
			name: "untagged name gets the default tag",
			tags: []string{"funny_name"},
			want: []string{"funny_name:latest"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vfs := newTestVFS(t)
			l := NewBuilder(vfs).Build()

			op := api.IndexedLoadDeployOperation{
				LoadDeployOperation: api.LoadDeployOperation{
					BaseCommandOperation: api.BaseCommandOperation{
						Command:  "load",
						RootKind: "manifest",
						Root:     api.Descriptor{Digest: vfs.manifestHash.String()},
						Manifests: []api.ManifestDeployInfo{{
							Descriptor: api.Descriptor{Digest: vfs.manifestHash.String()},
						}},
					},
					Registry:   tc.registry,
					Repository: tc.repository,
					Tags:       tc.tags,
					Daemon:     "docker",
				},
			}

			var buf bytes.Buffer
			tags, err := l.streamDockerTar(context.Background(), op, &buf)
			if err != nil {
				t.Fatalf("streamDockerTar: %v", err)
			}
			if strings.Join(tags, ",") != strings.Join(tc.want, ",") {
				t.Errorf("returned tags = %v, want %v", tags, tc.want)
			}
			if got := repoTagsFromTar(t, buf.Bytes()); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("manifest.json RepoTags = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStreamDockerTarRejectsInvalidName verifies the load path refuses a name no
// daemon could resolve instead of writing it into the tarball.
func TestStreamDockerTarRejectsInvalidName(t *testing.T) {
	vfs := newTestVFS(t)
	l := NewBuilder(vfs).Build()

	op := api.IndexedLoadDeployOperation{
		LoadDeployOperation: api.LoadDeployOperation{
			BaseCommandOperation: api.BaseCommandOperation{
				Command:  "load",
				RootKind: "manifest",
				Root:     api.Descriptor{Digest: vfs.manifestHash.String()},
				Manifests: []api.ManifestDeployInfo{{
					Descriptor: api.Descriptor{Digest: vfs.manifestHash.String()},
				}},
			},
			Registry:   "docker.mycompany.tld:1234",
			Repository: "foo",
			Tags:       []string{"no spaces allowed"},
			Daemon:     "docker",
		},
	}

	var buf bytes.Buffer
	if _, err := l.streamDockerTar(context.Background(), op, &buf); err == nil {
		t.Fatal("streamDockerTar accepted an invalid image reference")
	}
}
