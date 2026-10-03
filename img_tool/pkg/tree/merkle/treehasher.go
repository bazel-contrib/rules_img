package merkle

import (
	"bytes"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"path"
	"runtime"
	"sync"
)

type treeHasher struct {
	fs        fs.FS
	newHash   func() hash.Hash
	workers   chan struct{}
	fileNodes map[string]FileNode
	contents  map[string][]byte
	remaining int64
	fileMux   sync.Mutex
	mux       sync.Mutex
}

const maxCachedFileSize = 4 * 1024

// Tree hashers are short-lived; reuse scratch buffers across tree artifacts.
var copyBuffers = sync.Pool{New: func() any {
	return new([32 * 1024]byte)
}}

func NewTreeHasher(fsys fs.FS, newHash func() hash.Hash) *treeHasher {
	return &treeHasher{
		fs:        fsys,
		newHash:   newHash,
		workers:   make(chan struct{}, min(4, runtime.GOMAXPROCS(0))-1),
		fileNodes: make(map[string]FileNode),
	}
}

// File returns metadata and any cached payload (nil on a cache miss). The data
// is read-only; use it after Build succeeds and while the filesystem is unchanged.
func (t *treeHasher) File(p string) (FileNode, []byte, bool) {
	t.mux.Lock()
	defer t.mux.Unlock()
	node, ok := t.fileNodes[p]
	return node, t.contents[string(node.ContentHash)], ok
}

func (t *treeHasher) Build() ([]byte, error) {
	return t.BuildWithContentCache(0)
}

// BuildWithContentCache retains small payloads for a subsequent archive pass.
// The payload budget charges at least 64 bytes per entry, including empty files.
func (t *treeHasher) BuildWithContentCache(maxBytes int64) ([]byte, error) {
	t.mux.Lock()
	defer t.mux.Unlock()
	clear(t.fileNodes)
	t.contents = nil
	t.remaining = maxBytes
	if maxBytes > 0 {
		t.contents = make(map[string][]byte)
	}
	return t.hashDirectory(".")
}

func (t *treeHasher) hashDirectory(p string) ([]byte, error) {
	children, err := fs.ReadDir(t.fs, p)
	if err != nil {
		return nil, fmt.Errorf("reading directory %s: %w", p, err)
	}
	if p != "." && len(children) == 0 {
		// Bazel does not preserve empty directories in tree artifacts across
		// local and remote execution. Only the declared root may be empty.
		return nil, fmt.Errorf("empty directory %s in tree artifact", p)
	}

	files := make([]FileNode, len(children))
	digests := make([][]byte, len(children))
	errs := make([]error, len(children))
	var pending sync.WaitGroup
	for i, child := range children {
		childPath := path.Join(p, child.Name())
		collect := func() {
			switch {
			case child.Type().IsRegular():
				info, err := child.Info()
				if err != nil {
					errs[i] = fmt.Errorf("collecting %s: %w", childPath, err)
					return
				}
				files[i], errs[i] = t.collectRegularFile(childPath, info)
			case child.IsDir():
				digests[i], errs[i] = t.hashDirectory(childPath)
			default:
				errs[i] = fmt.Errorf("collecting %s: unsupported file type %v", childPath, child.Type().String())
			}
		}
		// Count the caller as a worker. Run inline when full so recursive
		// directory traversal cannot deadlock waiting for a worker slot.
		select {
		case t.workers <- struct{}{}:
			pending.Go(func() {
				defer func() { <-t.workers }()
				collect()
			})
		default:
			collect()
		}
	}
	pending.Wait()

	var directory Directory
	// Preserve ReadDir's sorted order regardless of worker completion order.
	for i, child := range children {
		if errs[i] != nil {
			return nil, errs[i]
		}
		if child.IsDir() {
			directory.Directories = append(directory.Directories, DirectoryNode{
				Name: metadataString(child.Name()),
				Hash: digests[i],
			})
		} else {
			directory.Files = append(directory.Files, files[i])
		}
	}
	return directory.Hash(t.newHash()), nil
}

func (t *treeHasher) collectRegularFile(p string, i fs.FileInfo) (FileNode, error) {
	if path.Base(p) != i.Name() {
		// This indicates a symlink which we didn't intend to follow
		// or a bad implementation of fs.FS.
		return FileNode{}, errors.New("file name does not match path base")
	}
	f, err := t.fs.Open(p)
	if err != nil {
		return FileNode{}, err
	}
	defer f.Close()

	contentHasher := t.newHash()
	buf := copyBuffers.Get().(*[32 * 1024]byte)
	defer copyBuffers.Put(buf)
	cachedSize := -1
	if t.contents != nil && i.Size() <= maxCachedFileSize {
		// Read one extra byte so a stale size cannot truncate the hash or cache.
		n, err := io.ReadFull(f, buf[:maxCachedFileSize+1])
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return FileNode{}, err
		}
		if _, err := contentHasher.Write(buf[:n]); err != nil {
			return FileNode{}, err
		}
		if n <= maxCachedFileSize {
			cachedSize = n
		}
	}
	// Hide WriterTo so os.File does not allocate its own buffer for every file.
	if cachedSize < 0 {
		if _, err := io.CopyBuffer(contentHasher, struct{ io.Reader }{f}, buf[:]); err != nil {
			return FileNode{}, err
		}
	}
	node := DefaultFileNode(contentHasher.Sum(nil), i)
	t.fileMux.Lock()
	t.fileNodes[p] = node
	if cachedSize >= 0 {
		key := string(node.ContentHash)
		cost := int64(max(cachedSize, 64))
		if _, exists := t.contents[key]; !exists && cost <= t.remaining {
			t.contents[key] = bytes.Clone(buf[:cachedSize])
			t.remaining -= cost
		}
	}
	t.fileMux.Unlock()
	return node, nil
}
