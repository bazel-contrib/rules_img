// Copyright 2026 The rules_img Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"bytes"
	"encoding/json"
	"sync"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Kind classifies a stored manifest so that its references can be traced
// without trusting the Content-Type a client happened to send.
type Kind uint8

const (
	// KindOther is an artifact or otherwise unrecognized manifest. Its config
	// and layer descriptors are traced if it has any.
	KindOther Kind = iota
	// KindManifest is an image manifest: it references a config blob and layer
	// blobs, and may declare a subject.
	KindManifest
	// KindIndex is an image index: it references other manifests or indexes,
	// and may declare a subject.
	KindIndex
)

// Manifest is a manifest as stored by a Store.
type Manifest struct {
	// ContentType is the media type the manifest is served with.
	ContentType string
	// Kind says how Blob's references are laid out. It is determined once, when
	// the manifest is stored.
	Kind Kind
	// Blob is the raw manifest, byte for byte as the client pushed it. Its
	// digest is the key it is stored under.
	Blob []byte
}

// Store holds a registry's manifests, keyed by digest, and its tags, which
// point at those digests. Blob *content* belongs to a BlobHandler; a Store
// never holds layer bytes.
//
// Storing manifests by digest alone -- with tags as pointers rather than second
// copies -- is what keeps a tag and the manifest it names from drifting apart.
// Two copies of the same bytes, one under the tag and one under the digest, are
// two references a Collector could expire independently of one another.
//
// Implementations must be safe for concurrent use. Every operation is
// self-contained: a Store offers no transactions, so callers must tolerate
// another client's write landing between two of their own calls.
type Store interface {
	// HasRepo reports whether the repository holds any manifest or tag. It
	// exists so handlers can tell NAME_UNKNOWN from MANIFEST_UNKNOWN.
	HasRepo(repo string) bool
	// RangeRepos calls fn for every repository that holds a manifest or a tag,
	// stopping early if fn returns false.
	RangeRepos(fn func(repo string) bool)

	// GetManifest returns the manifest stored under digest in repo.
	GetManifest(repo string, digest v1.Hash) (Manifest, bool)
	// PutManifest stores a manifest under its digest, replacing any manifest
	// already stored under it.
	PutManifest(repo string, digest v1.Hash, manifest Manifest)
	// DeleteManifest removes the manifest stored under digest. Tags pointing at
	// it are left alone; the caller decides what to do with them.
	DeleteManifest(repo string, digest v1.Hash)
	// RangeManifests calls fn for every manifest in repo, stopping early if fn
	// returns false.
	RangeManifests(repo string, fn func(digest v1.Hash, manifest Manifest) bool)

	// ResolveTag returns the digest a tag points at.
	ResolveTag(repo, tag string) (v1.Hash, bool)
	// PutTag points a tag at a digest, replacing where it pointed before. The
	// digest need not be stored yet.
	PutTag(repo, tag string, digest v1.Hash)
	// DeleteTag removes a tag. The manifest it pointed at is left alone.
	DeleteTag(repo, tag string)
	// RangeTags calls fn for every tag in repo, stopping early if fn returns
	// false.
	RangeTags(repo string, fn func(tag string, digest v1.Hash) bool)
}

// memStore is the default Store: everything in memory, for the lifetime of the
// process, unless a Collector is configured to evict.
type memStore struct {
	lock sync.RWMutex
	// repos maps repository -> digest -> manifest.
	repos map[string]map[v1.Hash]Manifest
	// tags maps repository -> tag -> digest.
	tags map[string]map[string]v1.Hash
}

// NewMemStore returns an in-memory Store.
func NewMemStore() Store {
	return &memStore{
		repos: make(map[string]map[v1.Hash]Manifest),
		tags:  make(map[string]map[string]v1.Hash),
	}
}

func (s *memStore) HasRepo(repo string) bool {
	s.lock.RLock()
	defer s.lock.RUnlock()

	return len(s.repos[repo]) > 0 || len(s.tags[repo]) > 0
}

func (s *memStore) RangeRepos(fn func(repo string) bool) {
	for _, repo := range s.repoNames() {
		if !fn(repo) {
			return
		}
	}
}

// repoNames snapshots the repository names so that fn -- which may call back
// into the Store, or block -- runs without the lock held.
func (s *memStore) repoNames() []string {
	s.lock.RLock()
	defer s.lock.RUnlock()

	names := make([]string, 0, len(s.repos))
	for repo := range s.repos {
		names = append(names, repo)
	}
	for repo := range s.tags {
		if _, ok := s.repos[repo]; !ok {
			names = append(names, repo)
		}
	}
	return names
}

func (s *memStore) GetManifest(repo string, digest v1.Hash) (Manifest, bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	manifest, ok := s.repos[repo][digest]
	return manifest, ok
}

func (s *memStore) PutManifest(repo string, digest v1.Hash, manifest Manifest) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if _, ok := s.repos[repo]; !ok {
		s.repos[repo] = make(map[v1.Hash]Manifest, 2)
	}
	s.repos[repo][digest] = manifest
}

func (s *memStore) DeleteManifest(repo string, digest v1.Hash) {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.repos[repo], digest)
	if len(s.repos[repo]) == 0 {
		delete(s.repos, repo)
	}
}

func (s *memStore) RangeManifests(repo string, fn func(v1.Hash, Manifest) bool) {
	s.lock.RLock()
	manifests := make(map[v1.Hash]Manifest, len(s.repos[repo]))
	for digest, manifest := range s.repos[repo] {
		manifests[digest] = manifest
	}
	s.lock.RUnlock()

	for digest, manifest := range manifests {
		if !fn(digest, manifest) {
			return
		}
	}
}

func (s *memStore) ResolveTag(repo, tag string) (v1.Hash, bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	digest, ok := s.tags[repo][tag]
	return digest, ok
}

func (s *memStore) PutTag(repo, tag string, digest v1.Hash) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if _, ok := s.tags[repo]; !ok {
		s.tags[repo] = make(map[string]v1.Hash, 2)
	}
	s.tags[repo][tag] = digest
}

func (s *memStore) DeleteTag(repo, tag string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.tags[repo], tag)
	if len(s.tags[repo]) == 0 {
		delete(s.tags, repo)
	}
}

func (s *memStore) RangeTags(repo string, fn func(string, v1.Hash) bool) {
	s.lock.RLock()
	tags := make(map[string]v1.Hash, len(s.tags[repo]))
	for tag, digest := range s.tags[repo] {
		tags[tag] = digest
	}
	s.lock.RUnlock()

	for tag, digest := range tags {
		if !fn(tag, digest) {
			return
		}
	}
}

// kindOf classifies a manifest about to be stored.
//
// Content outranks transport metadata. A manifest's own mediaType is covered by
// its digest and is what every consumer parses; its shape is too. The
// Content-Type header is neither, and clients do get it wrong. Misreading an
// index as a plain manifest is the mistake with teeth -- it hides the children
// the index needs from the collector -- so the header is consulted last.
func kindOf(contentType string, blob []byte) Kind {
	fromHeader := kindOfMediaType(types.MediaType(contentType))

	var body struct {
		MediaType string            `json:"mediaType"`
		Manifests []json.RawMessage `json:"manifests"`
		Config    *json.RawMessage  `json:"config"`
		Layers    []json.RawMessage `json:"layers"`
	}
	if err := json.Unmarshal(blob, &body); err != nil {
		return fromHeader
	}
	if kind := kindOfMediaType(types.MediaType(body.MediaType)); kind != KindOther {
		return kind
	}
	switch {
	case body.Manifests != nil:
		return KindIndex
	case body.Config != nil || body.Layers != nil:
		return KindManifest
	}
	return fromHeader
}

func kindOfMediaType(mediaType types.MediaType) Kind {
	switch {
	case mediaType.IsIndex():
		return KindIndex
	case mediaType.IsImage():
		return KindManifest
	}
	return KindOther
}

// references are the objects a single manifest points at.
type references struct {
	// manifests are child manifests or indexes, in the same repository.
	manifests []v1.Hash
	// blobs are config and layer blobs.
	blobs []descriptor
	// subject is the manifest this one is a referrer of, if any.
	subject *v1.Hash
}

// descriptor is the part of a v1.Descriptor the collector cares about.
type descriptor struct {
	digest v1.Hash
	size   int64
}

// parseReferences reads out everything a stored manifest points at. Unparsable
// manifests yield no references at all, which is the conservative answer for a
// leaf: it can still be collected on its own terms, but it drags nothing down
// with it.
func parseReferences(manifest Manifest) references {
	var refs references
	switch manifest.Kind {
	case KindIndex:
		index, err := v1.ParseIndexManifest(bytes.NewReader(manifest.Blob))
		if err != nil {
			return refs
		}
		for _, desc := range index.Manifests {
			if desc.MediaType.IsIndex() || desc.MediaType.IsImage() {
				refs.manifests = append(refs.manifests, desc.Digest)
				continue
			}
			refs.blobs = append(refs.blobs, descriptor{digest: desc.Digest, size: desc.Size})
		}
		if index.Subject != nil {
			subject := index.Subject.Digest
			refs.subject = &subject
		}
	default:
		image, err := v1.ParseManifest(bytes.NewReader(manifest.Blob))
		if err != nil {
			return refs
		}
		if image.Config.Digest != (v1.Hash{}) {
			refs.blobs = append(refs.blobs, descriptor{digest: image.Config.Digest, size: image.Config.Size})
		}
		for _, layer := range image.Layers {
			refs.blobs = append(refs.blobs, descriptor{digest: layer.Digest, size: layer.Size})
		}
		if image.Subject != nil {
			subject := image.Subject.Digest
			refs.subject = &subject
		}
	}
	return refs
}
