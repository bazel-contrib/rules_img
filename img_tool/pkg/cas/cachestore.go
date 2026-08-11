package cas

import (
	"container/list"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// tempDirName is the subdirectory of the cache root that holds partially
	// fetched blobs. It deliberately sits next to (not inside) cas/ so that a
	// half-written blob is never mistaken for a cache entry -- neither by us nor
	// by Bazel, when the root is a shared Bazel disk cache.
	tempDirName = "img-cas-tmp"

	// tempMaxAge is how long a temp file must be untouched before a later run
	// deletes it. Anything younger may belong to a concurrently running process.
	tempMaxAge = 24 * time.Hour

	dirPerm  = 0o755
	filePerm = 0o644
)

// DiskCacheBlobPath returns the path of a CAS blob inside a Bazel disk cache
// rooted at cacheDir. The layout is Bazel's: cas/<first two hex chars>/<hex>,
// with no digest function in the path.
func DiskCacheBlobPath(cacheDir string, hexDigest string) string {
	return filepath.Join(cacheDir, "cas", hexDigest[:2], hexDigest)
}

// diskStore is the on-disk half of a CachingReader: a Bazel-disk-cache-shaped
// directory plus an in-memory index of the blobs it may evict from it.
//
// The index only ever holds entries this store put there -- blobs it wrote, and,
// when a size limit applies, the entries found by the startup scan of a
// directory we manage. A shared Bazel disk cache is never scanned, so Bazel's
// own entries never become eviction candidates; Bazel's disk cache GC owns those.
type diskStore struct {
	dir string
	// ephemeral marks a directory this process created because no user cache
	// directory was available. close removes it; a configured directory survives.
	ephemeral bool
	maxSize   int64 // 0: unlimited, no scan and no proactive eviction

	// write is indirected so tests can inject write failures (ENOSPC).
	write func(f *os.File, p []byte) (int, error)

	mu        sync.Mutex
	entries   map[string]*list.Element // blob key -> element of lru
	lru       *list.List               // *storeEntry, most recently used at the front
	totalSize int64                    // bytes tracked in entries
	reserved  int64                    // bytes promised to in-flight fetches
	disabled  bool                     // disk caching gave up
	evicted   uint64
}

// storeEntry is one evictable blob in the index.
type storeEntry struct {
	key  string
	path string
	size int64
}

// newDiskStore prepares dir for use: creates the cas/ and temp directories,
// removes stale temp files, and, if a size limit applies, indexes the blobs
// already there and evicts down to the limit.
func newDiskStore(dir string, ephemeral bool, maxSize int64) (*diskStore, error) {
	s := &diskStore{
		dir:       dir,
		ephemeral: ephemeral,
		maxSize:   maxSize,
		write:     func(f *os.File, p []byte) (int, error) { return f.Write(p) },
		entries:   make(map[string]*list.Element),
		lru:       list.New(),
	}
	if err := os.MkdirAll(filepath.Join(dir, "cas"), dirPerm); err != nil {
		return nil, fmt.Errorf("creating CAS cache directory: %w", err)
	}
	if err := os.MkdirAll(s.tempDir(), dirPerm); err != nil {
		return nil, fmt.Errorf("creating CAS cache temp directory: %w", err)
	}
	s.sweepTemp()
	if maxSize > 0 {
		s.scan()
		s.prune()
	}
	return s, nil
}

func (s *diskStore) tempDir() string {
	return filepath.Join(s.dir, tempDirName)
}

func (s *diskStore) blobPath(d Digest) string {
	return DiskCacheBlobPath(s.dir, hex.EncodeToString(d.Hash))
}

// scan indexes the blobs already in the cache directory, oldest first, so that
// the size limit covers entries written by earlier runs. It is only called for
// directories this store manages (see newDiskStore).
func (s *diskStore) scan() {
	type found struct {
		entry   storeEntry
		modTime time.Time
	}
	var blobs []found
	casDir := filepath.Join(s.dir, "cas")
	shards, err := os.ReadDir(casDir)
	if err != nil {
		return
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(casDir, shard.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			info, err := file.Info()
			if err != nil || info.IsDir() {
				continue
			}
			blobs = append(blobs, found{
				entry: storeEntry{
					key:  file.Name(),
					path: filepath.Join(casDir, shard.Name(), file.Name()),
					size: info.Size(),
				},
				modTime: info.ModTime(),
			})
		}
	}
	// Insert oldest first so the LRU order reflects the recorded access times.
	slices.SortStableFunc(blobs, func(a, b found) int { return a.modTime.Compare(b.modTime) })

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, blob := range blobs {
		if _, ok := s.entries[blob.entry.key]; ok {
			continue
		}
		entry := blob.entry
		s.entries[entry.key] = s.lru.PushFront(&entry)
		s.totalSize += entry.size
	}
}

// sweepTemp removes temp files left behind by a killed process. Files younger
// than tempMaxAge are left alone: they may belong to a running process.
func (s *diskStore) sweepTemp() {
	files, err := os.ReadDir(s.tempDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-tempMaxAge)
	for _, file := range files {
		info, err := file.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		os.Remove(filepath.Join(s.tempDir(), file.Name()))
	}
}

// open returns a reader for a blob that is already in the cache directory. It
// fails if the blob is absent or has the wrong size (a truncated or foreign file
// under that name).
func (s *diskStore) open(d Digest) (*os.File, error) {
	path := s.blobPath(d)
	// Record the access before opening the file: it keeps blobs we are still
	// using in our LRU and in Bazel's disk cache GC, and on Windows a file's
	// times cannot be set while we hold a read handle on it.
	now := time.Now()
	os.Chtimes(path, now, now)

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.Size() != d.SizeBytes {
		f.Close()
		return nil, fmt.Errorf("cached blob %s has size %d, want %d", path, info.Size(), d.SizeBytes)
	}
	s.touch(d)
	return f, nil
}

// has reports whether the blob is in the cache directory with the expected size.
func (s *diskStore) has(d Digest) bool {
	info, err := os.Stat(s.blobPath(d))
	return err == nil && info.Size() == d.SizeBytes
}

// touch moves an indexed blob to the front of the LRU. Blobs we did not put in
// the index (a shared Bazel disk cache's own entries) stay untracked.
func (s *diskStore) touch(d Digest) {
	key := hex.EncodeToString(d.Hash)
	s.mu.Lock()
	defer s.mu.Unlock()
	if element, ok := s.entries[key]; ok {
		s.lru.MoveToFront(element)
	}
}

// createTemp opens a file for a blob being fetched and reserves size bytes
// against the limit, evicting first if necessary. The caller must eventually
// call finalize or discard.
func (s *diskStore) createTemp(size int64) (*os.File, error) {
	s.mu.Lock()
	disabled := s.disabled
	s.mu.Unlock()
	if disabled {
		return nil, errDiskCacheDisabled
	}
	s.makeRoom(size)

	s.mu.Lock()
	s.reserved += size
	s.mu.Unlock()

	f, err := os.CreateTemp(s.tempDir(), "blob-*")
	if err != nil {
		s.release(size)
		return nil, err
	}
	// CreateTemp uses 0600; cache entries are world-readable like Bazel's, which
	// matters when the directory is shared between users.
	os.Chmod(f.Name(), filePerm)
	return f, nil
}

// release gives back a reservation made by createTemp.
func (s *diskStore) release(size int64) {
	s.mu.Lock()
	s.releaseLocked(size)
	s.mu.Unlock()
}

// releaseLocked implements release. s.mu must be held.
func (s *diskStore) releaseLocked(size int64) {
	s.reserved -= size
	if s.reserved < 0 {
		s.reserved = 0
	}
}

// discard removes a temp file and releases its reservation.
func (s *diskStore) discard(tempPath string, size int64) {
	os.Remove(tempPath)
	s.release(size)
}

// finalize moves a completely written temp file to its content-addressed path
// and indexes it. It releases the reservation either way.
func (s *diskStore) finalize(tempPath string, d Digest) error {
	path := s.blobPath(d)
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		s.discard(tempPath, d.SizeBytes)
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		// Another process may have won the race (or, on Windows, hold the
		// destination open). Identical content under the same digest means the
		// entry is there, which is all we wanted.
		os.Remove(tempPath)
		if !s.has(d) {
			s.release(d.SizeBytes)
			return err
		}
	}

	key := hex.EncodeToString(d.Hash)
	s.mu.Lock()
	if element, ok := s.entries[key]; ok {
		s.lru.MoveToFront(element)
	} else {
		s.entries[key] = s.lru.PushFront(&storeEntry{key: key, path: path, size: d.SizeBytes})
		s.totalSize += d.SizeBytes
	}
	// The blob is accounted for by the index now, so drop the reservation before
	// pruning -- otherwise it would be counted twice and evict a blob too many.
	s.releaseLocked(d.SizeBytes)
	s.mu.Unlock()

	s.prune()
	return nil
}

// writeAll writes p to f, making room by evicting cached blobs when the file
// system reports it is out of space. The bytes already written stay valid, so a
// retry never needs to re-fetch anything.
func (s *diskStore) writeAll(f *os.File, p []byte) error {
	for len(p) > 0 {
		n, err := s.write(f, p)
		p = p[n:]
		switch {
		case err == nil && n == 0:
			return io.ErrShortWrite
		case err == nil:
			continue // short write, keep going
		case !isNoSpace(err):
			return err
		}
		// Out of space: free at least what is left to write, then retry. Eviction
		// can only free a finite amount, so this terminates.
		if freed := s.freeAtLeast(int64(len(p))); freed == 0 {
			return err
		}
	}
	return nil
}

// evict removes least recently used blobs while more reports that eviction
// should continue, and returns the number of bytes freed. s.mu must be held.
//
// Entries we fail to delete (another process holds the file open on Windows)
// stay indexed and accounted for; eviction just moves on to the next candidate.
func (s *diskStore) evictLocked(more func(freed int64) bool) int64 {
	var freed int64
	// Walk from the least recently used entry towards the most recently used.
	for element := s.lru.Back(); element != nil && more(freed); {
		prev := element.Prev()
		entry := element.Value.(*storeEntry)
		err := os.Remove(entry.path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			s.lru.Remove(element)
			delete(s.entries, entry.key)
			s.totalSize -= entry.size
			if err == nil {
				freed += entry.size
				s.evicted++
			}
		}
		element = prev
	}
	return freed
}

// makeRoom evicts so that size more bytes fit under the size limit. Without a
// limit nothing is evicted: a cache directory we do not manage (a Bazel disk
// cache) is only ever pruned when the file system actually runs out of space.
func (s *diskStore) makeRoom(size int64) {
	if s.maxSize <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked(func(int64) bool { return s.totalSize+s.reserved+size > s.maxSize })
}

// prune brings the cache back under the size limit.
func (s *diskStore) prune() {
	if s.maxSize <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked(func(int64) bool { return s.totalSize+s.reserved > s.maxSize })
}

// freeAtLeast evicts, regardless of any size limit, until want bytes have been
// freed or there is nothing left to evict. It returns the bytes freed. This is
// the out-of-space emergency: the size limit is not what is binding.
func (s *diskStore) freeAtLeast(want int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evictLocked(func(freed int64) bool { return freed < want })
}

// disable turns off disk caching for the rest of the process. Reads keep
// working: the caching reader falls back to streaming straight from upstream.
func (s *diskStore) disable(reason error) {
	s.mu.Lock()
	alreadyDisabled := s.disabled
	s.disabled = true
	s.mu.Unlock()
	if !alreadyDisabled {
		fmt.Fprintf(os.Stderr, "WARNING: cannot write to CAS blob cache %s (%v); continuing without local caching\n", s.dir, reason)
	}
}

func (s *diskStore) isDisabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disabled
}

func (s *diskStore) evictions() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.evicted
}

// close removes the cache directory if this process created it as a temporary
// stand-in. Persistent directories are left for the next run.
func (s *diskStore) close() error {
	if s.ephemeral {
		return os.RemoveAll(s.dir)
	}
	return nil
}

// errDiskCacheDisabled reports that a blob cannot be cached locally. It is never
// returned to callers of the CachingReader -- it makes them fall back to reading
// straight from upstream.
var errDiskCacheDisabled = errors.New("cas: disk caching disabled")

// isNoSpace reports whether err is the file system refusing a write for lack of
// space. Go maps this to ENOSPC on Unix; Windows reports ERROR_DISK_FULL /
// ERROR_HANDLE_DISK_FULL, which have no errno equivalent, so their messages are
// matched as well.
func isNoSpace(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ENOSPC) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no space left on device") ||
		strings.Contains(msg, "not enough space on the disk") ||
		strings.Contains(msg, "disk is full")
}
