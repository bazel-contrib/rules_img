package cas

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// writeCachedBlob puts content in a cache directory the way the store would,
// with the given modification time, and returns its digest.
func writeCachedBlob(t *testing.T, dir string, content []byte, modTime time.Time) Digest {
	t.Helper()
	digest := blobDigest(content)
	path := DiskCacheBlobPath(dir, digest.hexHash())
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, filePerm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestDiskCacheBlobPath(t *testing.T) {
	// The layout must match Bazel's disk cache, which is what makes sharing a
	// directory with Bazel (IMG_DISK_CACHE) work in both directions.
	got := DiskCacheBlobPath("/cache", "abcdef0123456789")
	want := filepath.Join("/cache", "cas", "ab", "abcdef0123456789")
	if got != want {
		t.Errorf("DiskCacheBlobPath = %q, want %q", got, want)
	}
}

func TestResolveCacheDirUsesConfiguredDir(t *testing.T) {
	dir, ephemeral, err := resolveCacheDir("/some/dir")
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/some/dir" || ephemeral {
		t.Errorf("resolveCacheDir = (%q, %v), want the configured directory and no cleanup", dir, ephemeral)
	}
}

func TestResolveCacheDirDefaultsToUserCacheDir(t *testing.T) {
	userCacheDir, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache directory: %v", err)
	}
	dir, ephemeral, err := resolveCacheDir("")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(userCacheDir, cacheDirName); dir != want {
		t.Errorf("resolveCacheDir = %q, want %q", dir, want)
	}
	if ephemeral {
		t.Error("the user cache directory should persist across runs")
	}
	if _, err := os.Stat(dir); err == nil {
		t.Log("note: the default cache directory already exists on this machine")
	}
}

func TestDiskStoreCloseRemovesOnlyEphemeralDir(t *testing.T) {
	persistent := t.TempDir()
	store, err := newDiskStore(persistent, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(persistent); err != nil {
		t.Errorf("a configured cache directory should survive Close: %v", err)
	}

	ephemeral := filepath.Join(t.TempDir(), "throwaway")
	store, err = newDiskStore(ephemeral, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ephemeral); !errors.Is(err, os.ErrNotExist) {
		t.Error("a temporary cache directory should be removed by Close")
	}
}

func TestDiskStoreEnforcesMaxSizeOnExistingDir(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	oldest := writeCachedBlob(t, dir, blobContent(30, 1024), now.Add(-2*time.Hour))
	newer := writeCachedBlob(t, dir, blobContent(31, 1024), now.Add(-1*time.Hour))
	newest := writeCachedBlob(t, dir, blobContent(32, 1024), now)

	// A directory we manage is indexed at startup, so the limit covers what
	// earlier runs left behind.
	store, err := newDiskStore(dir, false, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if store.has(oldest) {
		t.Error("the oldest blob should have been evicted at startup")
	}
	for _, digest := range []Digest{newer, newest} {
		if !store.has(digest) {
			t.Error("a recent blob was evicted at startup")
		}
	}
	if store.evictions() != 1 {
		t.Errorf("evicted %d blobs, want 1", store.evictions())
	}
}

func TestDiskStoreWithoutMaxSizeLeavesExistingBlobsAlone(t *testing.T) {
	dir := t.TempDir()
	// A Bazel disk cache is full of entries that are none of our business: with no
	// size limit they are neither indexed nor evictable.
	foreign := writeCachedBlob(t, dir, blobContent(33, 4096), time.Now().Add(-time.Hour))

	store, err := newDiskStore(dir, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !store.has(foreign) {
		t.Fatal("an existing blob was removed from an unmanaged cache directory")
	}
	if freed := store.freeAtLeast(4096); freed != 0 {
		t.Errorf("freed %d bytes from unindexed blobs, want 0", freed)
	}
	if !store.has(foreign) {
		t.Error("eviction removed a blob this process did not write")
	}
}

func TestDiskStoreSweepsStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	tempDir := filepath.Join(dir, tempDirName)
	if err := os.MkdirAll(tempDir, dirPerm); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(tempDir, "blob-stale")
	fresh := filepath.Join(tempDir, "blob-fresh")
	for _, path := range []string{stale, fresh} {
		if err := os.WriteFile(path, []byte("partial"), filePerm); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * tempMaxAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := newDiskStore(dir, false, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Error("a temp file left behind by an earlier run should be swept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a recent temp file may belong to a running process: %v", err)
	}
}

func TestDiskStoreOpenRejectsWrongSize(t *testing.T) {
	dir := t.TempDir()
	store, err := newDiskStore(dir, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	content := blobContent(34, 512)
	digest := writeCachedBlob(t, dir, content, time.Now())

	file, err := store.open(digest)
	if err != nil {
		t.Fatalf("opening a cached blob: %v", err)
	}
	file.Close()

	// A file of the wrong length under a digest's name is not that blob.
	truncated := Digest{algorithm: digest.algorithm, Hash: digest.Hash, SizeBytes: digest.SizeBytes + 1}
	if _, err := store.open(truncated); err == nil {
		t.Error("opening a blob with a mismatched size succeeded")
	}
}

func TestIsNoSpace(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "errno", err: &os.PathError{Op: "write", Err: syscall.ENOSPC}, want: true},
		{name: "unix message", err: errors.New("write /tmp/blob: no space left on device"), want: true},
		{name: "windows message", err: errors.New("write blob: There is not enough space on the disk."), want: true},
		{name: "other errno", err: &os.PathError{Op: "write", Err: syscall.EACCES}, want: false},
		{name: "unrelated", err: fmt.Errorf("connection reset"), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isNoSpace(test.err); got != test.want {
				t.Errorf("isNoSpace(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
