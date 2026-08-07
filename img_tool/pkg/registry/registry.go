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

// Package registry implements a docker V2 registry and the OCI distribution specification.
//
// It is designed to be used anywhere a low dependency container registry is needed, with an
// initial focus on tests.
//
// Its goal is to be standards compliant and its strictness will increase over time.
//
// This is currently a low flightmiles system. It's likely quite safe to use in tests; If you're using it
// in production, please let us know how and send us CL's for integration tests.
package registry

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

type registry struct {
	log              *log.Logger
	blobs            blobs
	manifests        manifests
	referrersEnabled bool
	warnings         map[float64]string

	store       Store
	collector   *Collector
	blobPruning bool
}

// https://docs.docker.com/registry/spec/api/#api-version-check
// https://github.com/opencontainers/distribution-spec/blob/master/spec.md#api-version-check
func (r *registry) v2(resp http.ResponseWriter, req *http.Request) *regError {
	if r.warnings != nil {
		rnd := rand.Float64()
		for prob, msg := range r.warnings {
			if prob > rnd {
				resp.Header().Add("Warning", fmt.Sprintf(`299 - "%s"`, msg))
			}
		}
	}

	if isBlob(req) {
		return r.blobs.handle(resp, req)
	}
	if isManifest(req) {
		return r.manifests.handle(resp, req)
	}
	if isTags(req) {
		return r.manifests.handleTags(resp, req)
	}
	if isCatalog(req) {
		return r.manifests.handleCatalog(resp, req)
	}
	if r.referrersEnabled && isReferrers(req) {
		return r.manifests.handleReferrers(resp, req)
	}
	resp.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if req.URL.Path != "/v2/" && req.URL.Path != "/v2" {
		return &regError{
			Status:  http.StatusNotFound,
			Code:    "METHOD_UNKNOWN",
			Message: "We don't understand your method + url",
		}
	}
	resp.WriteHeader(200)
	return nil
}

func (r *registry) root(resp http.ResponseWriter, req *http.Request) {
	if rerr := r.v2(resp, req); rerr != nil {
		r.log.Printf("%s %s %d %s %s", req.Method, req.URL, rerr.Status, rerr.Code, rerr.Message)
		rerr.Write(resp)
		return
	}
	r.log.Printf("%s %s", req.Method, req.URL)
}

// New returns a handler which implements the docker registry protocol.
// It should be registered at the site root.
func New(opts ...Option) http.Handler {
	r := &registry{
		log: log.New(os.Stderr, "", log.LstdFlags),
		blobs: blobs{
			blobHandler: &memHandler{m: map[string][]byte{}},
			uploads:     map[string][]byte{},
			log:         log.New(os.Stderr, "", log.LstdFlags),
		},
		manifests:   manifests{log: log.New(os.Stderr, "", log.LstdFlags)},
		blobPruning: true,
	}
	for _, o := range opts {
		o(r)
	}

	switch {
	case r.store == nil && r.collector == nil:
		r.store = NewMemStore()
	case r.store == nil:
		r.store = r.collector.Store()
	case r.collector != nil && r.collector.Store() != r.store:
		panic("registry: WithCollector was given a different Store than WithStore")
	}
	r.manifests.store = r.store
	r.manifests.collector = r.collector
	r.blobs.collector = r.collector
	if r.blobPruning {
		if deleter, ok := r.blobs.blobHandler.(BlobDeleteHandler); ok {
			// A blob nothing references any more is the registry's to reclaim,
			// but only where the handler allows deleting: storage shared with
			// something else -- Bazel's CAS, say -- must be left alone.
			r.collector.OnBlobCollected(func(repo string, digest v1.Hash) {
				if err := deleter.Delete(context.Background(), repo, digest); err != nil {
					r.log.Printf("collecting blob %s: %v", digest, err)
				}
			})
		}
	}
	return http.HandlerFunc(r.root)
}

// Option describes the available options
// for creating the registry.
type Option func(r *registry)

// Logger overrides the logger used to record requests to the registry.
func Logger(l *log.Logger) Option {
	return func(r *registry) {
		r.log = l
		r.manifests.log = l
		r.blobs.log = l
	}
}

// WithReferrersSupport enables the referrers API endpoint (OCI 1.1+)
func WithReferrersSupport(enabled bool) Option {
	return func(r *registry) {
		r.referrersEnabled = enabled
	}
}

func WithWarning(prob float64, msg string) Option {
	return func(r *registry) {
		if r.warnings == nil {
			r.warnings = map[float64]string{}
		}
		r.warnings[prob] = msg
	}
}

func WithBlobHandler(h BlobHandler) Option {
	return func(r *registry) {
		r.blobs.blobHandler = h
	}
}

func WithBlobCreatedCallback(cb func(repo string, digest v1.Hash)) Option {
	return func(r *registry) {
		r.blobs.createdCallback = cb
	}
}

func WithBlobDeletedCallback(cb func(repo string, digest v1.Hash)) Option {
	return func(r *registry) {
		r.blobs.deletedCallback = cb
	}
}

func WithManifestPutCallback(cb func(repo, target, contentType string, blob []byte) error) Option {
	return func(r *registry) {
		r.manifests.putCallback = cb
	}
}

func WithManifestDeleteCallback(cb func(repo, target, contentType string, blob []byte) error) Option {
	return func(r *registry) {
		r.manifests.deleteCallback = cb
	}
}

// WithStore sets the store holding manifests and tags. It defaults to
// NewMemStore, which keeps them for the lifetime of the process.
func WithStore(store Store) Option {
	return func(r *registry) {
		r.store = store
	}
}

// WithCollector enables eviction. The Collector must have been created over the
// same Store the registry uses; passing only WithCollector adopts its store.
// Without a Collector nothing is ever evicted and the registry keeps no
// bookkeeping for it at all.
func WithCollector(collector *Collector) Option {
	return func(r *registry) {
		r.collector = collector
	}
}

// WithBlobPruning controls whether collecting a blob also deletes its contents,
// for blob handlers that support deletion. It is on by default. Turn it off for
// a handler whose storage is shared with something outside this registry.
func WithBlobPruning(enabled bool) Option {
	return func(r *registry) {
		r.blobPruning = enabled
	}
}
