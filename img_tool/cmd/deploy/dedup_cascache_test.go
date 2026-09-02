package deploy

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/deployvfs"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/push"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// These tests cover how the deduplicated push and the local blob cache in front
// of the remote CAS divide the work. They fix different halves of the same
// problem: pushing K images that share a layer to K repositories used to download
// that layer K times and upload it K times. The cache collapses the downloads; the
// deduplicated push collapses the uploads -- and, because a cross-mounted layer's
// bytes are never read, removes the extra downloads before the cache ever sees
// them.

// countingBlobSource is a cas.BlobSource serving blobs from memory and counting
// the reads that reach it, so a test can tell an upstream fetch from a local cache
// hit.
type countingBlobSource struct {
	mu    sync.Mutex
	blobs map[string][]byte
	reads map[string]int
}

func newCountingBlobSource(blobs map[string][]byte) *countingBlobSource {
	return &countingBlobSource{blobs: blobs, reads: map[string]int{}}
}

func (s *countingBlobSource) FindMissingBlobs(_ context.Context, digests []cas.Digest) ([]cas.Digest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var missing []cas.Digest
	for _, digest := range digests {
		if _, found := s.blobs[hex.EncodeToString(digest.Hash)]; !found {
			missing = append(missing, digest)
		}
	}
	return missing, nil
}

func (s *countingBlobSource) ReadBlob(_ context.Context, digest cas.Digest) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hex.EncodeToString(digest.Hash)
	content, found := s.blobs[key]
	if !found {
		return nil, fmt.Errorf("blob %s not in the fake CAS", key)
	}
	s.reads[key]++
	return content, nil
}

func (s *countingBlobSource) ReaderForBlob(_ context.Context, digest cas.Digest) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := hex.EncodeToString(digest.Hash)
	content, found := s.blobs[key]
	if !found {
		return nil, fmt.Errorf("blob %s not in the fake CAS", key)
	}
	s.reads[key]++
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (s *countingBlobSource) ReaderForBlobs(ctx context.Context, digests []cas.Digest) (io.ReadCloser, error) {
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

// readsOf returns how many times the blob with the given "sha256:<hex>" digest was
// read from the fake CAS.
func (s *countingBlobSource) readsOf(digest string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, hexDigest, _ := strings.Cut(digest, ":")
	return s.reads[hexDigest]
}

// casDeployVFS builds a VFS that resolves every blob -- manifests, configs and
// layers -- from the remote CAS through the real local blob cache, which is what a
// lazy push does.
func casDeployVFS(t *testing.T, dm api.DeployManifest, source cas.BlobSource) (*deployvfs.VFS, *cas.CachingReader) {
	t.Helper()
	cache, err := cas.NewCachingReader(source, cas.WithCacheDir(t.TempDir()))
	if err != nil {
		t.Fatalf("creating the local blob cache: %v", err)
	}
	t.Cleanup(func() { cache.Close() })
	vfs, err := deployvfs.NewBuilder(dm).WithCASReader(cache).Build()
	if err != nil {
		t.Fatalf("building VFS: %v", err)
	}
	return vfs, cache
}

// TestDeduplicatedPushReadsEachBlobFromTheCASOnce verifies that the deduplicated
// push leaves the local blob cache nothing to do for a shared layer: the layer is
// read once, for the single upload to its home repository, and the repositories
// that cross-mount it never open a reader at all.
func TestDeduplicatedPushReadsEachBlobFromTheCASOnce(t *testing.T) {
	reg := newNaiveRegistry()
	_, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 3, 2)
	source := newCountingBlobSource(images.blobs)
	vfs, cache := casDeployVFS(t, dm, source)

	pushOps, err := dm.PushOperations()
	if err != nil {
		t.Fatalf("reading push operations: %v", err)
	}
	ctx := context.Background()
	transport := reg.transport()
	views, err := prepareDedupPush(ctx, vfs, pushOps, nil, dedupOptions{
		jobs:          4,
		pushTransport: transport,
	})
	if err != nil {
		t.Fatalf("preparing the deduplicated push: %v", err)
	}
	if _, err := push.NewBuilder(vfs).
		WithVFSForOperation(func(op api.IndexedPushDeployOperation) push.VFS {
			return views.For(op.Registry, op.BaseCommandOperation)
		}).
		WithJobs(4).
		WithRemoteOptions(registryopts.Default().WithTransport(transport).Remote()...).
		Build().
		PushAll(ctx, pushOps, dm.Settings.PushStrategy); err != nil {
		t.Fatalf("deduplicated push: %v", err)
	}

	for _, digest := range images.shared {
		if got := source.readsOf(digest); got != 1 {
			t.Errorf("shared layer %s read %d times from the CAS, want exactly 1", digest, got)
		}
		if got := reg.countBlobPuts(digest); got != 1 {
			t.Errorf("shared layer %s uploaded %d times, want exactly 1", digest, got)
		}
	}
	// Nothing was read twice, so the cache had nothing to deduplicate or serve: the
	// deduplicated push removed those reads instead of caching them.
	stats := cache.Stats()
	if stats.Deduped != 0 || stats.Hits != 0 {
		t.Errorf("local blob cache saw %d deduplicated reads and %d hits, want none: the push should not have asked twice",
			stats.Deduped, stats.Hits)
	}
	if stats.Fetches == 0 {
		t.Error("local blob cache fetched nothing, so this test did not exercise the CAS path")
	}
}

// TestPlainPushLetsTheCacheAbsorbRepeatedReads is the other half of the pair: with
// the deduplicated push off, the manifest push asks for a shared layer once per
// destination repository, and the local blob cache keeps that from becoming several
// CAS downloads. Both features are needed -- the cache saves the downloads, the
// deduplicated push saves the uploads.
func TestPlainPushLetsTheCacheAbsorbRepeatedReads(t *testing.T) {
	reg := newNaiveRegistry()
	_, dm, images := buildSharedLayerLayouts(t, "reg.example.com", 3, 2)
	source := newCountingBlobSource(images.blobs)
	vfs, cache := casDeployVFS(t, dm, source)

	pushOps, err := dm.PushOperations()
	if err != nil {
		t.Fatalf("reading push operations: %v", err)
	}
	if _, err := push.NewBuilder(vfs).
		WithJobs(4).
		WithRemoteOptions(registryopts.Default().WithTransport(reg.transport()).Remote()...).
		Build().
		PushAll(context.Background(), pushOps, dm.Settings.PushStrategy); err != nil {
		t.Fatalf("plain push: %v", err)
	}

	for _, digest := range images.shared {
		// The cache absorbed the repeated reads, so the CAS saw fewer downloads than
		// the repositories that asked for the layer -- one, normally. Not "exactly
		// one", because the cache is built to answer a local problem by reading
		// upstream again rather than by failing: a rename Windows refuses while a
		// handle is open, an eviction, a lost file. Insisting on the best case here
		// makes the test fail on a platform where the cache did exactly what it
		// promises.
		if got := source.readsOf(digest); got >= len(images.repositories) {
			t.Errorf("shared layer %s read %d times from the CAS, want fewer than the %d repositories that need it (the cache should absorb the rest)",
				digest, got, len(images.repositories))
		}
		// Every repository still uploaded its own copy, which is what the
		// deduplicated push is for.
		if got := reg.countBlobPuts(digest); got != len(images.repositories) {
			t.Errorf("shared layer %s uploaded %d times, want once per repository (%d)", digest, got, len(images.repositories))
		}
	}
	stats := cache.Stats()
	if stats.Hits+stats.Deduped == 0 {
		t.Error("local blob cache neither served nor deduplicated a read, but the push asked for shared layers repeatedly")
	}
}
