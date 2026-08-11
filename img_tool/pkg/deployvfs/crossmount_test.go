package deployvfs

import (
	"bytes"
	"io"
	"strings"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

const crossMountDigest = "sha256:5555555555555555555555555555555555555555555555555555555555555555"

func crossMountLayerDesc() api.Descriptor {
	return api.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    crossMountDigest,
		Size:      3,
	}
}

func nopOpener() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte("abc"))), nil
}

// TestVFSLayerAppliesCrossMountHint verifies that a registered cross-mount source
// makes VFS.Layer return a *remote.MountableLayer referencing that repository,
// while VFS.RawLayer returns the plain (unwrapped) layer.
func TestVFSLayerAppliesCrossMountHint(t *testing.T) {
	desc := crossMountLayerDesc()
	h, err := registryv1.NewHash(desc.Digest)
	if err != nil {
		t.Fatalf("parsing digest: %v", err)
	}
	vfs := &VFS{
		blobs: map[string]blobEntry{
			desc.Digest: {Descriptor: desc, Location: "file", Opener: nopOpener},
		},
		crossMountHints: map[string]api.CrossMountSource{
			desc.Digest: {Registry: "reg.example.com", Repository: "staging"},
		},
		stats: &Stats{},
	}

	layer, err := vfs.Layer(h)
	if err != nil {
		t.Fatalf("Layer: %v", err)
	}
	ml, ok := layer.(*remote.MountableLayer)
	if !ok {
		t.Fatalf("Layer returned %T, want *remote.MountableLayer", layer)
	}
	if got := ml.Reference.Context().RepositoryStr(); got != "staging" {
		t.Errorf("mount reference repository = %q, want %q", got, "staging")
	}
	if got := ml.Reference.Context().RegistryStr(); got != "reg.example.com" {
		t.Errorf("mount reference registry = %q, want %q", got, "reg.example.com")
	}

	raw, err := vfs.RawLayer(h)
	if err != nil {
		t.Fatalf("RawLayer: %v", err)
	}
	if _, ok := raw.(*remote.MountableLayer); ok {
		t.Errorf("RawLayer returned a *remote.MountableLayer, want an unwrapped layer")
	}
}

// TestBuilderMergesExtraCrossMountHints verifies WithCrossMountSource is merged
// into the VFS via ingest (here exercised through layerBlob's stub fallback).
func TestLayerBlobStubsWhenCrossMountRegistered(t *testing.T) {
	desc := crossMountLayerDesc()
	b := NewBuilder(api.DeployManifest{}).WithCrossMountSource(desc.Digest, api.CrossMountSource{
		Registry:   "reg.example.com",
		Repository: "staging",
	})
	// extraCrossMountHints is merged into crossMountHints during ingest; layerBlob
	// reads it directly for the stub fallback.
	entry, err := b.layerBlob(0, 0, 0, "eager", api.LayerBlob{Descriptor: desc})
	if err != nil {
		t.Fatalf("layerBlob with cross-mount hint should not error under eager: %v", err)
	}
	if entry.Location != "stub" {
		t.Errorf("entry.Location = %q, want %q (blob is mounted, not fetched)", entry.Location, "stub")
	}
}

// TestLayerBlobEagerErrorsWithoutSourceOrHint is the control case: without any
// source or cross-mount hint, an eager layer must fail to resolve.
func TestLayerBlobEagerErrorsWithoutSourceOrHint(t *testing.T) {
	desc := crossMountLayerDesc()
	b := NewBuilder(api.DeployManifest{})
	if _, err := b.layerBlob(0, 0, 0, "eager", api.LayerBlob{Descriptor: desc}); err == nil {
		t.Error("expected an error resolving an eager layer with no source and no cross-mount hint")
	}
}

// TestNewLayer verifies the exported single-blob layer constructor reports the
// descriptor's digest/size/media type without re-hashing.
func TestNewLayer(t *testing.T) {
	desc := api.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    crossMountDigest,
		Size:      42,
	}
	layer := NewLayer(desc, nopOpener)
	if d, err := layer.Digest(); err != nil || d.String() != desc.Digest {
		t.Errorf("Digest() = (%v, %v), want %s", d, err, desc.Digest)
	}
	if s, err := layer.Size(); err != nil || s != 42 {
		t.Errorf("Size() = (%d, %v), want 42", s, err)
	}
	if mt, err := layer.MediaType(); err != nil || string(mt) != desc.MediaType {
		t.Errorf("MediaType() = (%q, %v), want %s", mt, err, desc.MediaType)
	}
}

// TestForbidLayerPush verifies that when Settings.ForbidLayerPush is set, VFS.Layer
// returns a layer whose byte access (Compressed/Uncompressed) fails while its
// metadata (Digest/Size/MediaType) still works, and that a cross-mount hint still
// wraps it in a *remote.MountableLayer so a server-side mount remains possible.
func TestForbidLayerPush(t *testing.T) {
	desc := crossMountLayerDesc()
	h, err := registryv1.NewHash(desc.Digest)
	if err != nil {
		t.Fatalf("parsing digest: %v", err)
	}
	vfs := &VFS{
		dm: api.DeployManifest{Settings: api.DeploySettings{ForbidLayerPush: true}},
		blobs: map[string]blobEntry{
			desc.Digest: {Descriptor: desc, Location: "file", Opener: nopOpener},
		},
		stats: &Stats{},
	}

	layer, err := vfs.Layer(h)
	if err != nil {
		t.Fatalf("Layer: %v", err)
	}
	// Metadata still works (needed for the manifest and existence/mount checks).
	if d, err := layer.Digest(); err != nil || d.String() != desc.Digest {
		t.Errorf("Digest() = (%v, %v), want %s", d, err, desc.Digest)
	}
	// Byte access must fail so an accidental upload errors loudly.
	if _, err := layer.Compressed(); err == nil {
		t.Error("Compressed() should fail when ForbidLayerPush is set")
	}
	if _, err := layer.Uncompressed(); err == nil {
		t.Error("Uncompressed() should fail when ForbidLayerPush is set")
	}

	// With a cross-mount hint, the layer is still wrapped for server-side mount.
	vfs.crossMountHints = map[string]api.CrossMountSource{
		desc.Digest: {Registry: "reg.example.com", Repository: "staging"},
	}
	mountable, err := vfs.Layer(h)
	if err != nil {
		t.Fatalf("Layer (with hint): %v", err)
	}
	ml, ok := mountable.(*remote.MountableLayer)
	if !ok {
		t.Fatalf("Layer returned %T, want *remote.MountableLayer", mountable)
	}
	if _, err := ml.Compressed(); err == nil {
		t.Error("mountable layer Compressed() should fail when ForbidLayerPush is set")
	}
}

// secondCrossMountDigest is a second layer digest used by the WithCrossMountPlan
// tests, which need one mount-only blob and one ordinary one.
const secondCrossMountDigest = "sha256:6666666666666666666666666666666666666666666666666666666666666666"

// planTestVFS builds a VFS holding both cross-mount test blobs, plus a
// per-operation hint on the first one so the tests can check that a plan's
// sources take precedence over it.
func planTestVFS(t *testing.T) *VFS {
	t.Helper()
	first := crossMountLayerDesc()
	second := crossMountLayerDesc()
	second.Digest = secondCrossMountDigest
	return &VFS{
		blobs: map[string]blobEntry{
			first.Digest:  {Descriptor: first, Location: "file", Opener: nopOpener},
			second.Digest: {Descriptor: second, Location: "file", Opener: nopOpener},
		},
		crossMountHints: map[string]api.CrossMountSource{
			first.Digest: {Repository: "base"},
		},
		stats: &Stats{},
	}
}

func mustHash(t *testing.T, digest string) registryv1.Hash {
	t.Helper()
	h, err := registryv1.NewHash(digest)
	if err != nil {
		t.Fatalf("parsing digest %s: %v", digest, err)
	}
	return h
}

// TestWithCrossMountPlanMountOnly verifies that a digest listed in
// CrossMountPlan.MountOnly is served as a mountable layer whose metadata works
// but whose bytes cannot be read, with the plan's source overriding the VFS's own
// hint and naming the repository the mount should have come from.
func TestWithCrossMountPlanMountOnly(t *testing.T) {
	vfs := planTestVFS(t)
	digest := crossMountDigest
	view := vfs.WithCrossMountPlan(CrossMountPlan{
		Sources:   map[string]api.CrossMountSource{digest: {Repository: "team/service-0"}},
		MountOnly: map[string]struct{}{digest: {}},
	})

	layer, err := view.Layer(mustHash(t, digest))
	if err != nil {
		t.Fatalf("Layer: %v", err)
	}
	ml, ok := layer.(*remote.MountableLayer)
	if !ok {
		t.Fatalf("Layer returned %T, want *remote.MountableLayer", layer)
	}
	// The plan's source wins over the per-operation hint the VFS was built with.
	if got := ml.Reference.Context().RepositoryStr(); got != "team/service-0" {
		t.Errorf("mount reference repository = %q, want %q", got, "team/service-0")
	}
	// A registry-less source keeps the mount within the destination registry.
	if got := ml.Reference.Context().RegistryStr(); got != "" {
		t.Errorf("mount reference registry = %q, want empty", got)
	}
	// Metadata still works: the manifest and the mount request need it.
	if d, err := ml.Digest(); err != nil || d.String() != digest {
		t.Errorf("Digest() = (%v, %v), want %s", d, err, digest)
	}
	_, err = ml.Compressed()
	if err == nil {
		t.Fatal("Compressed() should fail for a mount-only layer")
	}
	// The error has to say where the blob was uploaded, or it is not actionable.
	if !strings.Contains(err.Error(), "team/service-0") {
		t.Errorf("Compressed() error = %q, want it to name the home repository", err)
	}
}

// TestWithCrossMountPlanLeavesOtherBlobsUploadable verifies that a blob the plan
// does not list as mount-only keeps its byte-upload fallback, and that the
// receiver VFS is unaffected by the view.
func TestWithCrossMountPlanLeavesOtherBlobsUploadable(t *testing.T) {
	vfs := planTestVFS(t)
	view := vfs.WithCrossMountPlan(CrossMountPlan{
		Sources:   map[string]api.CrossMountSource{crossMountDigest: {Repository: "shared-blobs"}},
		MountOnly: map[string]struct{}{crossMountDigest: {}},
	})

	// The second blob is not in the plan, so it keeps the VFS's behavior: no hint,
	// real bytes.
	other, err := view.Layer(mustHash(t, secondCrossMountDigest))
	if err != nil {
		t.Fatalf("Layer: %v", err)
	}
	if _, ok := other.(*remote.MountableLayer); ok {
		t.Error("Layer returned a *remote.MountableLayer for a blob with no cross-mount source")
	}
	rc, err := other.Compressed()
	if err != nil {
		t.Fatalf("Compressed() for a blob outside the plan: %v", err)
	}
	rc.Close()

	// The receiver keeps serving the staged blob's real bytes: that is what the
	// staging upload (via RawLayer) and any concurrent load operation need.
	original, err := vfs.Layer(mustHash(t, crossMountDigest))
	if err != nil {
		t.Fatalf("Layer on the receiver: %v", err)
	}
	ml, ok := original.(*remote.MountableLayer)
	if !ok {
		t.Fatalf("Layer on the receiver returned %T, want *remote.MountableLayer", original)
	}
	if got := ml.Reference.Context().RepositoryStr(); got != "base" {
		t.Errorf("receiver mount reference repository = %q, want the unmodified %q", got, "base")
	}
	rc, err = ml.Compressed()
	if err != nil {
		t.Fatalf("Compressed() on the receiver: %v", err)
	}
	rc.Close()

	raw, err := vfs.RawLayer(mustHash(t, crossMountDigest))
	if err != nil {
		t.Fatalf("RawLayer: %v", err)
	}
	rc, err = raw.Compressed()
	if err != nil {
		t.Fatalf("RawLayer Compressed(): %v", err)
	}
	rc.Close()
}

// TestWithCrossMountPlanSharesStats verifies the view reports into the same
// counters as the VFS it came from, so the deploy's blob-transfer summary covers
// both phases.
func TestWithCrossMountPlanSharesStats(t *testing.T) {
	vfs := planTestVFS(t)
	view := vfs.WithCrossMountPlan(CrossMountPlan{})
	if view.Stats() != vfs.Stats() {
		t.Error("view does not share the receiver's Stats")
	}
}
