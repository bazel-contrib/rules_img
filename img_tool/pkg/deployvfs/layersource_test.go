package deployvfs

import (
	"context"
	"io"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
)

const testRegistryDigest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"

func layerDesc() api.Descriptor {
	return api.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    testRegistryDigest,
		Size:      123,
	}
}

// TestLayerFromRegistryPerLayerSources verifies the source-selection routing of
// layerFromRegistry (the blob Opener itself performs network access and is not
// invoked here).
func TestLayerFromRegistryPerLayerSources(t *testing.T) {
	b := NewBuilder(api.DeployManifest{})
	sources := []api.LayerSource{
		{Registry: "index.docker.io", Repository: "library/ubuntu"},
		{Registry: "mirror.example.com", Repository: "library/ubuntu"},
	}

	entry, err := b.layerFromRegistry(sources, layerDesc())
	if err != nil {
		t.Fatalf("expected per-layer sources to resolve, got error: %v", err)
	}
	if entry.Location != "registry" {
		t.Errorf("entry.Location = %q, want %q", entry.Location, "registry")
	}
}

// TestLayerFromRegistryUnconfigured verifies that a layer with no upstream
// sources is not resolvable from a registry.
func TestLayerFromRegistryUnconfigured(t *testing.T) {
	b := NewBuilder(api.DeployManifest{})
	if _, err := b.layerFromRegistry(nil, layerDesc()); err == nil {
		t.Error("expected error when no sources are configured")
	} else if bse, ok := err.(*BlobSourceError); !ok || bse.Kind != BlobSourceUnconfigured {
		t.Errorf("expected BlobSourceUnconfigured, got %v", err)
	}
}

type contextRecordingCAS struct {
	contexts []context.Context
}

func (c *contextRecordingCAS) FindMissingBlobs(ctx context.Context, _ []cas.Digest) ([]cas.Digest, error) {
	c.contexts = append(c.contexts, ctx)
	return nil, nil
}

func (c *contextRecordingCAS) ReadBlob(context.Context, cas.Digest) ([]byte, error) {
	return nil, nil
}

func (c *contextRecordingCAS) ReaderForBlob(ctx context.Context, _ cas.Digest) (io.ReadCloser, error) {
	c.contexts = append(c.contexts, ctx)
	return io.NopCloser(nilReader{}), nil
}

func (c *contextRecordingCAS) ReaderForBlobs(ctx context.Context, _ []cas.Digest) (io.ReadCloser, error) {
	c.contexts = append(c.contexts, ctx)
	return io.NopCloser(nilReader{}), nil
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }

func TestLayerFromCASPropagatesBuilderContext(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "invocation")
	reader := &contextRecordingCAS{}
	b := NewBuilder(api.DeployManifest{}).WithContext(ctx).WithCASReader(reader)

	entry, err := b.layerFromCAS(layerDesc())
	if err != nil {
		t.Fatal(err)
	}
	blob, err := entry.Compressed()
	if err != nil {
		t.Fatal(err)
	}
	blob.Close()

	if len(reader.contexts) != 2 {
		t.Fatalf("CAS received %d contexts, want 2", len(reader.contexts))
	}
	for i, got := range reader.contexts {
		if got.Value(contextKey{}) != "invocation" {
			t.Errorf("CAS context %d did not preserve the builder context", i)
		}
	}
}
