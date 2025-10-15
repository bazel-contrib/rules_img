package digestfs

import (
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// HashProvider creates new hash instances for digest calculation
type HashProvider interface {
	New() hash.Hash
}

// DigestFile represents a file with cached digest calculation and seeking capabilities
type DigestFile interface {
	io.ReadSeeker
	io.Closer

	// Path returns the real path of the file (after resolving symlinks)
	Path() string

	// Size returns the file size
	Size() int64

	// Digest returns the cached digest of the file contents
	Digest() ([]byte, error)
}

// runningLookup represents an in-progress digest calculation
type runningLookup struct {
	ch     chan struct{} // closed when digest is ready
	digest []byte        // populated when calculation completes
	err    error         // error from calculation, if any
}

// FileSystem provides digest-cached file access with memoization
type FileSystem struct {
	mu             sync.Mutex
	digestCache    map[string][]byte         // realpath -> digest
	runningLookups map[string]*runningLookup // realpath -> running calculation

	statMu    sync.RWMutex
	statCache map[string]os.FileInfo // realpath -> stat info

	realpathMu    sync.RWMutex
	realpathCache map[string]string // path -> realpath

	hashProvider HashProvider
}

type cachedDigestFile struct {
	file     *os.File
	realPath string
	size     int64
	fs       *FileSystem
}

// New creates a new digest file system with the given hash provider
func New(hashProvider HashProvider) *FileSystem {
	return &FileSystem{
		digestCache:    make(map[string][]byte),
		runningLookups: make(map[string]*runningLookup),
		statCache:      make(map[string]os.FileInfo),
		realpathCache:  make(map[string]string),
		hashProvider:   hashProvider,
	}
}

// cacheKey returns the cached real path or resolves and caches it
func (fs *FileSystem) cacheKey(path string) (string, error) {
	if runtime.GOOS == "windows" {
		// Path handling on Windows is too complex.
		// Simply use the original path as cache key.
		return path, nil
	}

	fs.realpathMu.RLock()
	realPath, cached := fs.realpathCache[path]
	fs.realpathMu.RUnlock()

	if cached {
		return realPath, nil
	}

	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}

	fs.realpathMu.Lock()
	fs.realpathCache[path] = resolvedPath
	fs.realpathMu.Unlock()

	return resolvedPath, nil
}

// getStat returns the cached stat info or queries and caches it
func (fs *FileSystem) getStat(realPath string, file *os.File) (os.FileInfo, error) {
	fs.statMu.RLock()
	stat, cached := fs.statCache[realPath]
	fs.statMu.RUnlock()

	if cached {
		return stat, nil
	}

	fileStat, err := file.Stat()
	if err != nil {
		return nil, err
	}

	fs.statMu.Lock()
	fs.statCache[realPath] = fileStat
	fs.statMu.Unlock()

	return fileStat, nil
}

// OpenFile opens a file for reading with digest caching capabilities
func (fs *FileSystem) OpenFile(path string) (DigestFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	realPath, err := fs.cacheKey(path)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("failed to resolve real path for %s: %w", path, err)
	}

	stat, err := fs.getStat(realPath, file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("failed to stat file %s: %w", realPath, err)
	}

	return &cachedDigestFile{
		file:     file,
		realPath: realPath,
		size:     stat.Size(),
		fs:       fs,
	}, nil
}

func (f *cachedDigestFile) Read(p []byte) (n int, err error) {
	return f.file.Read(p)
}

func (f *cachedDigestFile) Seek(offset int64, whence int) (int64, error) {
	return f.file.Seek(offset, whence)
}

func (f *cachedDigestFile) Close() error {
	return f.file.Close()
}

func (f *cachedDigestFile) Path() string {
	return f.realPath
}

func (f *cachedDigestFile) Size() int64 {
	return f.size
}

func (f *cachedDigestFile) Digest() ([]byte, error) {
	f.fs.mu.Lock()

	// Check if digest is already cached
	if digest, exists := f.fs.digestCache[f.realPath]; exists {
		f.fs.mu.Unlock()
		return digest, nil
	}

	// Check if there's already a running lookup for this file
	if lookup, exists := f.fs.runningLookups[f.realPath]; exists {
		// There's already a calculation in progress, subscribe to it
		ch := lookup.ch
		f.fs.mu.Unlock()

		// Wait for the calculation to complete
		<-ch

		// Now get the result (the worker has populated the cache)
		return lookup.digest, lookup.err
	}

	// We need to start a new calculation
	lookup := &runningLookup{
		ch: make(chan struct{}),
	}
	f.fs.runningLookups[f.realPath] = lookup
	f.fs.mu.Unlock()

	// Perform the calculation (outside the lock)
	digest, err := f.calculateDigest()

	// Update the cache and notify waiters
	f.fs.mu.Lock()
	if err != nil {
		lookup.err = err
	} else {
		f.fs.digestCache[f.realPath] = digest
		lookup.digest = digest
	}
	delete(f.fs.runningLookups, f.realPath)
	f.fs.mu.Unlock()
	close(lookup.ch) // Notify all waiters

	return digest, err
}

func (f *cachedDigestFile) precache() error {
	f.fs.mu.Lock()

	// Check if digest is already cached
	if _, exists := f.fs.digestCache[f.realPath]; exists {
		f.fs.mu.Unlock()
		return nil
	}

	// Check if there's already a running lookup for this file
	if _, exists := f.fs.runningLookups[f.realPath]; exists {
		// There's already a calculation in progress. Nothing to do.
		f.fs.mu.Unlock()
		return nil
	}

	// We need to start a new calculation
	lookup := &runningLookup{
		ch: make(chan struct{}),
	}
	f.fs.runningLookups[f.realPath] = lookup
	f.fs.mu.Unlock()

	// Perform the calculation (outside the lock)
	digest, err := f.calculateDigest()

	// Update the cache and notify waiters
	f.fs.mu.Lock()
	if err != nil {
		lookup.err = err
	} else {
		f.fs.digestCache[f.realPath] = digest
		lookup.digest = digest
	}
	delete(f.fs.runningLookups, f.realPath)
	f.fs.mu.Unlock()
	close(lookup.ch) // Notify all waiters

	return err
}

func (f *cachedDigestFile) calculateDigest() ([]byte, error) {
	// Save current position and reset to start
	currentPos, err := f.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}

	if _, err := f.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	defer func() { _, _ = f.file.Seek(currentPos, io.SeekStart) }()

	// Calculate digest
	h := f.fs.hashProvider.New()
	if _, err := io.Copy(h, f.file); err != nil {
		return nil, err
	}

	return h.Sum(nil), nil
}

// InvalidateCache removes the cached digest for a file path
func (fs *FileSystem) InvalidateCache(path string) {
	realPath, err := fs.cacheKey(path)
	if err != nil {
		return
	}

	fs.mu.Lock()
	delete(fs.digestCache, realPath)
	fs.mu.Unlock()

	fs.statMu.Lock()
	delete(fs.statCache, realPath)
	fs.statMu.Unlock()
}

// GetCachedDigest returns the cached digest for a file path, if available
func (fs *FileSystem) GetCachedDigest(path string) ([]byte, bool) {
	realPath, err := fs.cacheKey(path)
	if err != nil {
		return nil, false
	}

	fs.mu.Lock()
	digest, exists := fs.digestCache[realPath]
	fs.mu.Unlock()

	return digest, exists
}

// CacheStats returns statistics about the digest cache
func (fs *FileSystem) CacheStats() (entries int, totalDigests int) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.digestCache), len(fs.digestCache)
}
