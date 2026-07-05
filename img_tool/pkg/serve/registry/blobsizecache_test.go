package registry

import (
	"testing"
	"time"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestBlobSizeCacheTTLEvictsExpiredEntry(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	cache := NewBlobSizeCache(
		WithBlobSizeCacheTTL(time.Minute),
		withBlobSizeCacheClock(func() time.Time { return now }),
	)
	hash := mustHash(t, "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	cache.Set(hash, 42)
	assertCachedSize(t, cache, hash, 42, true)

	now = now.Add(time.Minute + time.Nanosecond)

	assertCachedSize(t, cache, hash, 0, false)
}

func TestBlobSizeCacheTTLRefreshesOnSet(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	cache := NewBlobSizeCache(
		WithBlobSizeCacheTTL(time.Minute),
		withBlobSizeCacheClock(func() time.Time { return now }),
	)
	hash := mustHash(t, "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	cache.Set(hash, 42)
	now = now.Add(30 * time.Second)
	cache.Set(hash, 84)
	now = now.Add(30*time.Second + time.Nanosecond)

	assertCachedSize(t, cache, hash, 84, true)

	now = now.Add(30 * time.Second)

	assertCachedSize(t, cache, hash, 0, false)
}

func TestBlobSizeCacheTTLDisabledPreservesEntry(t *testing.T) {
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	cache := NewBlobSizeCache(
		WithBlobSizeCacheTTL(0),
		withBlobSizeCacheClock(func() time.Time { return now }),
	)
	hash := mustHash(t, "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")

	cache.Set(hash, 42)
	now = now.Add(365 * 24 * time.Hour)

	assertCachedSize(t, cache, hash, 42, true)
}

func TestBlobSizeCacheGetMiss(t *testing.T) {
	cache := NewBlobSizeCache()
	hash := mustHash(t, "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")

	assertCachedSize(t, cache, hash, 0, false)
}

func mustHash(t *testing.T, digest string) registryv1.Hash {
	t.Helper()
	hash, err := registryv1.NewHash(digest)
	if err != nil {
		t.Fatalf("parsing digest %q: %v", digest, err)
	}
	return hash
}

func assertCachedSize(t *testing.T, cache *BlobSizeCache, hash registryv1.Hash, wantSize int64, wantOK bool) {
	t.Helper()
	gotSize, gotOK := cache.Get(hash)
	if gotOK != wantOK {
		t.Fatalf("cache.Get(%s) got ok %t, want %t", hash.String(), gotOK, wantOK)
	}
	if gotSize != wantSize {
		t.Fatalf("cache.Get(%s) got size %d, want %d", hash.String(), gotSize, wantSize)
	}
}
