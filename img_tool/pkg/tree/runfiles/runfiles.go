package runfiles

import (
	"io/fs"
	"iter"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

type Node interface {
	Type() api.FileType
	Open() (fs.File, error)
	Tree() (fs.FS, error)
	Readlink() (string, error)
}

// PathNode extends Node with path information for optimization
type PathNode interface {
	Node
	Path() string
}

type runfilesFS struct {
	entries       map[string]Node
	sortedEntries []string
}

func NewRunfilesFS() *runfilesFS {
	return &runfilesFS{
		entries: make(map[string]Node),
	}
}

func (r *runfilesFS) Add(name string, entry Node) {
	r.sortedEntries = append(r.sortedEntries, name)
	r.entries[name] = entry
}

func (r *runfilesFS) Open(name string) (fs.File, error) {
	if entry, ok := r.entries[name]; ok {
		return entry.Open()
	}
	return nil, fs.ErrNotExist
}

func (r *runfilesFS) Items() iter.Seq2[string, Node] {
	return func(yield func(string, Node) bool) {
		for _, name := range r.sortedEntries {
			if entry, ok := r.entries[name]; ok {
				if !yield(name, entry) {
					break
				}
			}
		}
	}
}
