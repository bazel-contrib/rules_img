package merkle

import (
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
	hashers   sync.Pool
	workers   chan struct{}
	fileNodes map[string]FileNode
	fileMux   sync.Mutex
	mux       sync.Mutex
}

func NewTreeHasher(fsys fs.FS, newHash func() hash.Hash) *treeHasher {
	return &treeHasher{
		fs:        fsys,
		hashers:   sync.Pool{New: func() any { return newHash() }},
		workers:   make(chan struct{}, min(4, runtime.GOMAXPROCS(0))-1),
		fileNodes: make(map[string]FileNode),
	}
}

// File returns metadata recorded by a successful Build.
func (t *treeHasher) File(p string) (FileNode, bool) {
	t.mux.Lock()
	defer t.mux.Unlock()
	node, ok := t.fileNodes[p]
	return node, ok
}

func (t *treeHasher) Build() ([]byte, error) {
	t.mux.Lock()
	defer t.mux.Unlock()
	clear(t.fileNodes)
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
	h := t.hashers.Get().(hash.Hash)
	defer t.hashers.Put(h)
	h.Reset()
	return directory.Hash(h), nil
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

	contentHasher := t.hashers.Get().(hash.Hash)
	defer t.hashers.Put(contentHasher)
	contentHasher.Reset()
	if _, err := io.Copy(contentHasher, f); err != nil {
		return FileNode{}, err
	}
	node := DefaultFileNode(contentHasher.Sum(nil), i)
	t.fileMux.Lock()
	t.fileNodes[p] = node
	t.fileMux.Unlock()
	return node, nil
}
