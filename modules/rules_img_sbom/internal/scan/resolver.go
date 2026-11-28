package scan

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	stereoscopeFile "github.com/anchore/stereoscope/pkg/file"
	"github.com/anchore/stereoscope/pkg/filetree"
	"github.com/anchore/syft/syft/file"
)

// resolver is a file.Resolver over the files of one layer, indexed from its
// tar (see indexTar). It is syft's own filetree resolver for a tree that isn't
// backed by the host's filesystem (as for a squashfs): request and response
// paths are the tree's paths.
type resolver struct {
	tree    *filetree.FileTree
	index   filetree.Index
	search  filetree.Searcher
	openers map[stereoscopeFile.ID]func() (io.ReadCloser, error)
}

var _ file.Resolver = (*resolver)(nil)

func (r *resolver) HasPath(p string) bool {
	return r.tree.HasPath(stereoscopeFile.Path(p))
}

func (r *resolver) FilesByPath(paths ...string) ([]file.Location, error) {
	locations := make([]file.Location, 0)
	for _, p := range paths {
		ref, err := r.search.SearchByPath(p, filetree.FollowBasenameLinks)
		if err != nil || !ref.HasReference() {
			continue
		}
		entry, err := r.index.Get(*ref.Reference)
		if err != nil || entry.IsDir() {
			continue
		}
		locations = append(locations, file.NewVirtualLocationFromDirectory(string(ref.RealPath), p, *ref.Reference))
	}
	return locations, nil
}

func (r *resolver) FilesByGlob(patterns ...string) ([]file.Location, error) {
	seen := stereoscopeFile.NewFileReferenceSet()
	locations := make([]file.Location, 0)
	for _, pattern := range patterns {
		refs, err := r.search.SearchByGlob(pattern, filetree.FollowBasenameLinks)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if !ref.HasReference() || seen.Contains(*ref.Reference) {
				continue
			}
			entry, err := r.index.Get(*ref.Reference)
			if err != nil {
				return nil, fmt.Errorf("unable to get file metadata for reference %s: %w", ref.RealPath, err)
			}
			if entry.IsDir() {
				continue
			}
			seen.Add(*ref.Reference)
			locations = append(locations, file.NewVirtualLocationFromDirectory(string(ref.RealPath), string(ref.RequestPath), *ref.Reference))
		}
	}
	return locations, nil
}

func (r *resolver) FilesByMIMEType(types ...string) ([]file.Location, error) {
	seen := stereoscopeFile.NewFileReferenceSet()
	locations := make([]file.Location, 0)
	refs, err := r.search.SearchByMIMEType(types...)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if !ref.HasReference() || seen.Contains(*ref.Reference) {
			continue
		}
		seen.Add(*ref.Reference)
		locations = append(locations, file.NewVirtualLocationFromDirectory(string(ref.RealPath), string(ref.RequestPath), *ref.Reference))
	}
	return locations, nil
}

// RelativeFileByPath resolves path in the layer: there is only one layer, so a
// file is always "relative" to every other.
func (r *resolver) RelativeFileByPath(_ file.Location, p string) *file.Location {
	locations, err := r.FilesByPath(p)
	if err != nil || len(locations) == 0 {
		return nil
	}
	return &locations[0]
}

func (r *resolver) FileContentsByLocation(location file.Location) (io.ReadCloser, error) {
	if location.RealPath == "" {
		return nil, errors.New("empty path given")
	}
	entry, err := r.index.Get(location.Reference())
	if err != nil {
		return nil, err
	}
	if entry.Type == stereoscopeFile.TypeDirectory {
		return nil, fmt.Errorf("cannot read contents of non-file %q", location.Reference().RealPath)
	}
	ref := location.Reference()
	open, ok := r.openers[ref.ID()]
	if !ok {
		return nil, fmt.Errorf("%q has no contents: %w", location.RealPath, os.ErrNotExist)
	}
	return open()
}

func (r *resolver) AllLocations(ctx context.Context) <-chan file.Location {
	results := make(chan file.Location)
	go func() {
		defer close(results)
		for _, ref := range r.tree.AllFiles(stereoscopeFile.AllTypes()...) {
			select {
			case <-ctx.Done():
				return
			case results <- file.NewLocationFromDirectory(string(ref.RealPath), "", ref):
			}
		}
	}()
	return results
}

func (r *resolver) FileMetadataByLocation(location file.Location) (file.Metadata, error) {
	entry, err := r.index.Get(location.Reference())
	if err != nil {
		return file.Metadata{}, fmt.Errorf("location: %+v : %w", location, os.ErrNotExist)
	}
	return entry.Metadata, nil
}
