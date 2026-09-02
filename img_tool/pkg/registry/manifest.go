// Copyright 2018 Google LLC All Rights Reserved.
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
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type catalog struct {
	Repos []string `json:"repositories"`
}

type listTags struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type manifests struct {
	store          Store
	collector      *Collector
	putCallback    func(repo, target, contentType string, blob []byte) error
	deleteCallback func(repo, target, contentType string, blob []byte) error
	log            *log.Logger
}

func isManifest(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "manifests"
}

func isTags(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "tags"
}

func isCatalog(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 2 {
		return false
	}

	return elems[len(elems)-1] == "_catalog"
}

// Returns whether this url should be handled by the referrers handler
func isReferrers(req *http.Request) bool {
	elems := strings.Split(req.URL.Path, "/")
	elems = elems[1:]
	if len(elems) < 4 {
		return false
	}
	return elems[len(elems)-2] == "referrers"
}

var (
	errNameUnknown = &regError{
		Status:  http.StatusNotFound,
		Code:    "NAME_UNKNOWN",
		Message: "Unknown name",
	}
	errManifestUnknown = &regError{
		Status:  http.StatusNotFound,
		Code:    "MANIFEST_UNKNOWN",
		Message: "Unknown manifest",
	}
)

// resolve looks up a manifest by the reference a client asked for, which is
// either an immutable digest or a tag pointing at one.
func (m *manifests) resolve(repo, target string) (v1.Hash, Manifest, *regError) {
	if !m.store.HasRepo(repo) {
		return v1.Hash{}, Manifest{}, errNameUnknown
	}

	digest, err := v1.NewHash(target)
	if err != nil {
		resolved, ok := m.store.ResolveTag(repo, target)
		if !ok {
			return v1.Hash{}, Manifest{}, errManifestUnknown
		}
		digest = resolved
	}

	manifest, ok := m.store.GetManifest(repo, digest)
	if !ok {
		return v1.Hash{}, Manifest{}, errManifestUnknown
	}
	return digest, manifest, nil
}

// touch records that a reference was just used, so eviction keeps it and
// everything it points at.
func (m *manifests) touch(repo, target string, digest v1.Hash) {
	m.collector.TouchManifest(repo, digest)
	if target != digest.String() {
		m.collector.TouchTag(repo, target)
	}
}

// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pulling-an-image-manifest
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#pushing-an-image
func (m *manifests) handle(resp http.ResponseWriter, req *http.Request) *regError {
	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	target := elem[len(elem)-1]
	repo := strings.Join(elem[1:len(elem)-2], "/")

	m.collector.MaybeCollect()

	switch req.Method {
	case http.MethodGet, http.MethodHead:
		digest, manifest, rerr := m.resolve(repo, target)
		if rerr != nil {
			return rerr
		}
		m.touch(repo, target, digest)

		resp.Header().Set("Docker-Content-Digest", digest.String())
		resp.Header().Set("Content-Type", manifest.ContentType)
		resp.Header().Set("Content-Length", fmt.Sprint(len(manifest.Blob)))
		resp.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			io.Copy(resp, bytes.NewReader(manifest.Blob))
		}
		return nil

	case http.MethodPut:
		b := &bytes.Buffer{}
		io.Copy(b, req.Body)
		h, _, _ := v1.SHA256(bytes.NewReader(b.Bytes()))
		contentType := req.Header.Get("Content-Type")
		mf := Manifest{
			Blob:        b.Bytes(),
			ContentType: contentType,
			Kind:        kindOf(contentType, b.Bytes()),
		}

		// A client pushing by digest is telling us what it thinks it is
		// pushing. Storing it under a digest its bytes do not hash to would
		// leave a reference nobody can pull by any other name.
		if asDigest, err := v1.NewHash(target); err == nil && asDigest != h {
			return &regError{
				Status:  http.StatusBadRequest,
				Code:    "DIGEST_INVALID",
				Message: fmt.Sprintf("Manifest hashes to %s, not the requested %s", h, asDigest),
			}
		}

		// If the manifest is a manifest list, check that the manifest
		// list's constituent manifests are already uploaded.
		// This isn't strictly required by the registry API, but some
		// registries require this. Kind, not the Content-Type, decides: an
		// index whose header is wrong is still an index, and storing one whose
		// children are absent is how a dangling reference gets in.
		if mf.Kind == KindIndex {
			im, err := v1.ParseIndexManifest(bytes.NewReader(mf.Blob))
			if err != nil {
				return &regError{
					Status:  http.StatusBadRequest,
					Code:    "MANIFEST_INVALID",
					Message: err.Error(),
				}
			}
			for _, desc := range im.Manifests {
				if !desc.MediaType.IsDistributable() {
					continue
				}
				if desc.MediaType.IsIndex() || desc.MediaType.IsImage() {
					if _, found := m.store.GetManifest(repo, desc.Digest); !found {
						return &regError{
							Status:  http.StatusNotFound,
							Code:    "MANIFEST_UNKNOWN",
							Message: fmt.Sprintf("Sub-manifest %q not found", desc.Digest),
						}
					}
				} else {
					// TODO: Probably want to do an existence check for blobs.
					m.log.Printf("TODO: Check blobs for %q", desc.Digest)
				}
			}
		}

		if m.putCallback != nil {
			if err := m.putCallback(repo, target, mf.ContentType, mf.Blob); err != nil {
				return &regError{
					Status:  http.StatusInternalServerError,
					Code:    "INTERNAL_ERROR",
					Message: fmt.Sprintf("Error in callback: %v", err),
				}
			}
		}

		// A manifest is stored once, under its digest. A tag is a pointer to
		// that digest rather than a second copy, so the two cannot be evicted
		// independently of one another. Writing them is still two calls to the
		// Store, so the write is announced to the collector: a sweep that ran
		// between them would see a manifest no tag names yet.
		// See https://docs.docker.com/engine/reference/commandline/pull/#pull-an-image-by-digest-immutable-identifier.
		done := m.collector.writing()
		m.store.PutManifest(repo, h, mf)
		if target != h.String() {
			m.store.PutTag(repo, target, h)
		}
		m.touch(repo, target, h)
		// Record the blobs this manifest names, even ones no client has asked
		// for yet: it is what lets eviction reason about them, and what lets a
		// blob's metadata be dropped once nothing names it any more.
		for _, blob := range parseReferences(mf).blobs {
			m.collector.TouchBlob(repo, blob.digest, blob.size)
		}
		done()

		resp.Header().Set("Docker-Content-Digest", h.String())
		resp.WriteHeader(http.StatusCreated)
		return nil

	case http.MethodDelete:
		digest, manifest, rerr := m.resolve(repo, target)
		if rerr != nil {
			return rerr
		}

		if m.deleteCallback != nil {
			if err := m.deleteCallback(repo, target, manifest.ContentType, manifest.Blob); err != nil {
				return &regError{
					Status:  http.StatusInternalServerError,
					Code:    "INTERNAL_ERROR",
					Message: fmt.Sprintf("Error in callback: %v", err),
				}
			}
		}

		if target == digest.String() {
			// Deleting by digest deletes the manifest, so every tag resolving
			// to it goes too -- they would otherwise resolve to nothing.
			m.store.DeleteManifest(repo, digest)
			m.collector.ForgetManifest(repo, digest)
			for _, tag := range m.tagsFor(repo, digest) {
				m.store.DeleteTag(repo, tag)
				m.collector.ForgetTag(repo, tag)
			}
		} else {
			// Deleting a tag only untags: the manifest stays reachable by
			// digest until it is deleted or evicted.
			m.store.DeleteTag(repo, target)
			m.collector.ForgetTag(repo, target)
		}
		resp.WriteHeader(http.StatusAccepted)
		return nil

	default:
		return &regError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
}

// tagsFor returns the tags in repo that resolve to digest.
func (m *manifests) tagsFor(repo string, digest v1.Hash) []string {
	var tags []string
	m.store.RangeTags(repo, func(tag string, resolved v1.Hash) bool {
		if resolved == digest {
			tags = append(tags, tag)
		}
		return true
	})
	return tags
}

func (m *manifests) handleTags(resp http.ResponseWriter, req *http.Request) *regError {
	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	repo := strings.Join(elem[1:len(elem)-2], "/")

	if req.Method == "GET" {
		m.collector.MaybeCollect()

		if !m.store.HasRepo(repo) {
			return errNameUnknown
		}

		var tags []string
		m.store.RangeTags(repo, func(tag string, _ v1.Hash) bool {
			tags = append(tags, tag)
			return true
		})
		sort.Strings(tags)

		// https://github.com/opencontainers/distribution-spec/blob/b505e9cc53ec499edbd9c1be32298388921bb705/detail.md#tags-paginated
		// Offset using last query parameter.
		if last := req.URL.Query().Get("last"); last != "" {
			for i, t := range tags {
				if t > last {
					tags = tags[i:]
					break
				}
			}
		}

		// Limit using n query parameter.
		if ns := req.URL.Query().Get("n"); ns != "" {
			if n, err := strconv.Atoi(ns); err != nil {
				return &regError{
					Status:  http.StatusBadRequest,
					Code:    "BAD_REQUEST",
					Message: fmt.Sprintf("parsing n: %v", err),
				}
			} else if n < len(tags) {
				tags = tags[:n]
			}
		}

		tagsToList := listTags{
			Name: repo,
			Tags: tags,
		}

		msg, _ := json.Marshal(tagsToList)
		resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
		resp.WriteHeader(http.StatusOK)
		io.Copy(resp, bytes.NewReader([]byte(msg)))
		return nil
	}

	return &regError{
		Status:  http.StatusBadRequest,
		Code:    "METHOD_UNKNOWN",
		Message: "We don't understand your method + url",
	}
}

func (m *manifests) handleCatalog(resp http.ResponseWriter, req *http.Request) *regError {
	query := req.URL.Query()
	nStr := query.Get("n")
	n := 10000
	if nStr != "" {
		n, _ = strconv.Atoi(nStr)
	}

	if req.Method == "GET" {
		m.collector.MaybeCollect()

		var repos []string
		countRepos := 0
		// TODO: implement pagination
		m.store.RangeRepos(func(repo string) bool {
			if countRepos >= n {
				return false
			}
			countRepos++
			repos = append(repos, repo)
			return true
		})

		repositoriesToList := catalog{
			Repos: repos,
		}

		msg, _ := json.Marshal(repositoriesToList)
		resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
		resp.WriteHeader(http.StatusOK)
		io.Copy(resp, bytes.NewReader([]byte(msg)))
		return nil
	}

	return &regError{
		Status:  http.StatusBadRequest,
		Code:    "METHOD_UNKNOWN",
		Message: "We don't understand your method + url",
	}
}

// TODO: implement handling of artifactType querystring
func (m *manifests) handleReferrers(resp http.ResponseWriter, req *http.Request) *regError {
	// Ensure this is a GET request
	if req.Method != "GET" {
		return &regError{
			Status:  http.StatusBadRequest,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}

	elem := strings.Split(req.URL.Path, "/")
	elem = elem[1:]
	target := elem[len(elem)-1]
	repo := strings.Join(elem[1:len(elem)-2], "/")

	// Validate that incoming target is a valid digest
	if _, err := v1.NewHash(target); err != nil {
		return &regError{
			Status:  http.StatusBadRequest,
			Code:    "UNSUPPORTED",
			Message: "Target must be a valid digest",
		}
	}

	m.collector.MaybeCollect()

	if !m.store.HasRepo(repo) {
		return errNameUnknown
	}

	im := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		Manifests:     []v1.Descriptor{},
	}
	m.store.RangeManifests(repo, func(digest v1.Hash, manifest Manifest) bool {
		var refPointer struct {
			Subject *v1.Descriptor `json:"subject"`
		}
		json.Unmarshal(manifest.Blob, &refPointer)
		if refPointer.Subject == nil {
			return true
		}
		referenceDigest := refPointer.Subject.Digest
		if referenceDigest.String() != target {
			return true
		}
		// At this point, we know the current digest references the target.
		// Per the OCI spec, the referrers descriptor's artifactType is the
		// referring manifest's artifactType when present, otherwise the config
		// descriptor's mediaType. Annotations are propagated so clients can
		// filter referrers without fetching each manifest.
		var imageAsArtifact struct {
			ArtifactType string `json:"artifactType"`
			Config       struct {
				MediaType string `json:"mediaType"`
			} `json:"config"`
			Annotations map[string]string `json:"annotations"`
		}
		json.Unmarshal(manifest.Blob, &imageAsArtifact)
		artifactType := imageAsArtifact.ArtifactType
		if artifactType == "" {
			artifactType = imageAsArtifact.Config.MediaType
		}
		im.Manifests = append(im.Manifests, v1.Descriptor{
			MediaType:    types.MediaType(manifest.ContentType),
			Size:         int64(len(manifest.Blob)),
			Digest:       digest,
			ArtifactType: artifactType,
			Annotations:  imageAsArtifact.Annotations,
		})
		return true
	})
	msg, _ := json.Marshal(&im)
	resp.Header().Set("Content-Length", fmt.Sprint(len(msg)))
	resp.Header().Set("Content-Type", string(types.OCIImageIndex))
	resp.WriteHeader(http.StatusOK)
	io.Copy(resp, bytes.NewReader([]byte(msg)))
	return nil
}
