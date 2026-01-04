package merkle

import (
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
)

type treeHasher struct {
	fs      fs.FS
	newHash func() hash.Hash

	// fileNodes is a map of file nodes,
	// where the key is the path of the file
	// relative to the tree root.
	fileNodes map[string]FileNode

	// dirNodes is a map of directory nodes,
	// where the key is the path of the directory
	// relative to the tree root.
	dirNodes map[string]DirectoryNode

	// unfinishedFileNodes is a map of unfinished directory nodes.
	// The key is the path of the directory
	// relative to the tree root.
	// After all children of a directory are processed,
	// the directory node can be removed from this map
	// and added to the dirByLevel map.
	unfinishedDirByLevel map[int]map[string][]fs.DirEntry

	rootChildren []fs.DirEntry

	mux sync.Mutex
}

func NewTreeHasher(fsys fs.FS, newHash func() hash.Hash) *treeHasher {
	return &treeHasher{
		fs:                   fsys,
		newHash:              newHash,
		fileNodes:            make(map[string]FileNode),
		dirNodes:             make(map[string]DirectoryNode),
		unfinishedDirByLevel: make(map[int]map[string][]fs.DirEntry),
	}
}

func (t *treeHasher) Build() ([]byte, error) {
	t.mux.Lock()
	defer t.mux.Unlock()

	// In case someone calls Build() multiple times,
	// we need to clear the maps to ensure correctness.
	// We could allow users to cache trees,
	// in which case we would need to preserve the maps
	// between calls (if we believe that they are not stale).
	clear(t.fileNodes)
	clear(t.dirNodes)
	clear(t.unfinishedDirByLevel)
	t.rootChildren = nil

	if err := fs.WalkDir(t.fs, ".", t.walkdDirCollector); err != nil {
		return nil, fmt.Errorf("building tree for hashing: %w", err)
	}
	// Now we can build up our merkle tree from the leaves up.
	var maxLevel int
	for l := range t.unfinishedDirByLevel {
		if l > maxLevel {
			maxLevel = l
		}
	}
	for l := maxLevel; l > 0; l-- {
		for p, children := range t.unfinishedDirByLevel[l] {
			var files []FileNode
			var dirs []DirectoryNode
			for _, child := range children {
				if child.Type().IsRegular() {
					fileNode := t.fileNodes[path.Join(p, child.Name())]
					files = append(files, fileNode)
				} else if child.Type().IsDir() {
					dirNode := t.dirNodes[path.Join(p, child.Name())]
					dirs = append(dirs, dirNode)
				} else {
					return nil, fmt.Errorf("unsupported file type %v", child.Type().String())
				}
			}
			if len(files) == 0 && len(dirs) == 0 {
				// This is an empty directory, which constitutes a possible correctness issues:
				// Bazel doesn't track empty directories in tree artifacts correctly,
				// but actions can still produce them in real filesystems for actions that produce tree artifacts.
				// Including them in the artifact would be incorrect (Bazel sometimes provies them, i.e. when running locally),
				// but ignoring them would be incorrect too (maybe the action is producing them on purpose).
				return nil, fmt.Errorf("empty directory %s in tree artifact", p)
			}
			slices.SortFunc(files, func(a, b FileNode) int {
				return strings.Compare(string(a.Name), string(b.Name))
			})
			slices.SortFunc(dirs, func(a, b DirectoryNode) int {
				return strings.Compare(string(a.Name), string(b.Name))
			})

			directory := Directory{
				Files:       files,
				Directories: dirs,
			}
			dirHash := directory.Hash(t.newHash())
			dirNode := DirectoryNode{
				Name: metadataString(path.Base(p)),
				Hash: metadataBytes(dirHash),
			}
			t.dirNodes[p] = dirNode
		}
	}
	// Now we can build the root directory node.
	// This is allowed to be empty, to match Bazel's behavior
	// of ctx.actions.declare_directory.
	var files []FileNode
	var dirs []DirectoryNode
	for _, child := range t.rootChildren {
		if child.Type().IsRegular() {
			fileNode := t.fileNodes[child.Name()]
			files = append(files, fileNode)
		} else if child.Type().IsDir() {
			dirNode := t.dirNodes[child.Name()]
			dirs = append(dirs, dirNode)
		} else {
			return nil, fmt.Errorf("unsupported file type %v", child.Type().String())
		}
	}
	slices.SortFunc(files, func(a, b FileNode) int {
		return strings.Compare(string(a.Name), string(b.Name))
	})
	slices.SortFunc(dirs, func(a, b DirectoryNode) int {
		return strings.Compare(string(a.Name), string(b.Name))
	})
	directory := Directory{
		Files:       files,
		Directories: dirs,
	}
	return directory.Hash(t.newHash()), nil
}

// walkDirCollector is called by fs.WalkDir to collect file and directory nodes.
// It is used to build the (unfinished) tree structure.
// After fs.WalkDir is finished, the fileNodes and unfinishedDirByLevel maps
// are populated, but crucially, the dirNodes map is not yet populated.
// This is because we need to build up the merkle tree from the leaves up,
// while the fs.WalkDir function traverses the tree from the root down.
func (t *treeHasher) walkdDirCollector(p string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}
	dirFS, ok := t.fs.(fs.ReadDirFS)
	if !ok {
		return fmt.Errorf("tree hasher: filesystem does support listing directories: %w", err)
	}

	if p == "." {
		// If the path is the root directory, we simply record it in a special field.
		children, err := dirFS.ReadDir(p)
		if err != nil {
			return fmt.Errorf("collecting directory %s: %w", p, err)
		}
		t.rootChildren = children
		return nil
	}
	pathComponents := strings.Split(p, "/")
	level := len(pathComponents)
	info, err := d.Info()
	if err != nil {
		return fmt.Errorf("collecting %s: %w", p, err)
	}
	if d.Type().IsRegular() {
		// If the entry is a regular file, we need to collect it.
		return t.collectRegularFile(p, info)
	}
	if !d.Type().IsDir() {
		return fmt.Errorf("collecting %s: unsupported file type %v", p, d.Type().String())
	}

	// get a list of direct children of the directory
	children, err := dirFS.ReadDir(p)
	if err != nil {
		return fmt.Errorf("collecting directory %s: %w", p, err)
	}

	// If the entry is a directory, we need memorize it as an unfinished directory node.
	// We will populate the dirNodes map later.
	if _, ok := t.unfinishedDirByLevel[level]; !ok {
		t.unfinishedDirByLevel[level] = make(map[string][]fs.DirEntry)
	}
	t.unfinishedDirByLevel[level][p] = children
	return nil
}

func (t *treeHasher) collectRegularFile(p string, i fs.FileInfo) error {
	if path.Base(p) != i.Name() {
		// This indicates a symlink which we didn't intend to follow
		// or a bad implementation of fs.FS.
		return errors.New("file name does not match path base")
	}

	f, err := t.fs.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()

	contentHasher := t.newHash()
	if _, err := io.Copy(contentHasher, f); err != nil {
		return err
	}

	fileNode := DefaultFileNode(contentHasher.Sum(nil), i)
	t.fileNodes[p] = fileNode
	return nil
}
