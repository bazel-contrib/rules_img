package referrers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/sync/errgroup"
)

const (
	// headerSubject is set by a registry that processed the subject of a pushed
	// manifest, see
	// https://github.com/opencontainers/distribution-spec/blob/main/spec.md#pushing-manifests-with-subject.
	headerSubject = "OCI-Subject"
	// maxTagSchemaAttempts bounds how often a referrers tag is read, extended
	// and written back before giving up because other writers keep winning (or
	// the registry keeps losing the entry).
	maxTagSchemaAttempts = 10
	// tagSchemaBaseDelay is the wait before the first retry of a referrers tag
	// update whose precondition failed. It doubles with every attempt.
	tagSchemaBaseDelay = 50 * time.Millisecond
	// defaultJobs is how many blobs a Writer uploads at once unless WithJobs
	// says otherwise, as remote.Pusher does by default.
	defaultJobs = 4
)

// defaultBackoff retries a request a few times unless Options.Backoff says
// otherwise.
var defaultBackoff = remote.Backoff{Duration: time.Second, Factor: 3, Jitter: 0.1, Steps: 3}

// Writer pushes referrers: manifests that carry a subject. It writes them like
// remote.Pusher does, but maintains the referrers tag schema on registries
// without the referrers API itself, because go-containerregistry gets it wrong
// in two ways:
//
//   - It reads, extends and writes back the referrers tag unconditionally, so of
//     two referrers of one subject pushed at the same time, one is lost.
//   - It skips a manifest the registry already has, and so never adds it to the
//     tag when an earlier push failed to (or lost the race).
//
// A Writer always writes the manifest. If the registry doesn't confirm that it
// processed the subject (with the OCI-Subject response header), or confirms it
// but has no referrers API (as AWS ECR does), the Writer adds the manifest to
// the referrers tag of its subject, unless it's already listed there. It writes
// the tag back conditionally (If-Match with the ETag it read, or If-None-Match
// if there was no tag yet), retries when another writer got there first (412
// Precondition Failed), and reads the tag again to check that the manifest
// stayed in it. That last step also narrows the window for losing an entry on
// registries that ignore the condition.
//
// A Writer is safe for concurrent use.
type Writer struct {
	pusher  *remote.Pusher
	options Options
	jobs    int

	mu sync.Mutex
	// clients are the authenticated clients, by repository.
	clients map[string]*http.Client
	// api records, by repository, whether the registry's referrers API answers.
	api map[string]bool
}

// NewWriter returns a Writer that uploads blobs (and the children of an index)
// through pusher, and accesses the registry with options for everything else.
// Both should use the same transport and credentials.
func NewWriter(pusher *remote.Pusher, options Options) *Writer {
	return &Writer{
		pusher:  pusher,
		options: options,
		jobs:    defaultJobs,
		clients: map[string]*http.Client{},
		api:     map[string]bool{},
	}
}

// WithJobs sets how many blobs (or children of an index) the Writer uploads at
// once for a manifest. Non-positive values are ignored.
func (w *Writer) WithJobs(jobs int) *Writer {
	if jobs > 0 {
		w.jobs = jobs
	}
	return w
}

// referrerManifest is what a Writer reads from a manifest it pushes.
type referrerManifest struct {
	MediaType    types.MediaType   `json:"mediaType"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Config       *struct {
		MediaType string `json:"mediaType"`
	} `json:"config,omitempty"`
	Subject *v1.Descriptor `json:"subject,omitempty"`
}

// withMediaType is implemented by a remote.Taggable that knows its media type.
type withMediaType interface {
	MediaType() (types.MediaType, error)
}

// Push writes the manifest t, after its blobs (or, for an index, its children),
// to ref. If it has a subject, Push makes sure that the registry lists it among
// the referrers of the subject (see Writer).
func (w *Writer) Push(ctx context.Context, ref name.Reference, t remote.Taggable) error {
	raw, err := t.RawManifest()
	if err != nil {
		return fmt.Errorf("reading the manifest: %w", err)
	}
	digest, size, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	var manifest referrerManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return fmt.Errorf("parsing the manifest %s: %w", digest, err)
	}
	// As remote.Pusher determines it.
	mediaType := manifest.MediaType
	if t, ok := t.(withMediaType); ok {
		if mediaType, err = t.MediaType(); err != nil {
			return fmt.Errorf("reading the media type of the manifest %s: %w", digest, err)
		}
	}
	if mediaType == "" {
		mediaType = types.DockerManifestSchema2
	}

	repo := ref.Context()
	if err := w.writeDependencies(ctx, repo, t); err != nil {
		return fmt.Errorf("pushing the contents of %s: %w", digest, err)
	}
	client, err := w.client(ctx, repo)
	if err != nil {
		return err
	}
	processed, err := w.putManifest(ctx, client, repo, ref.Identifier(), mediaType, raw)
	if err != nil {
		return fmt.Errorf("pushing the manifest %s: %w", digest, err)
	}
	if manifest.Subject == nil {
		return nil
	}

	subject := manifest.Subject.Digest.String()
	if processed == subject {
		supported, err := w.referrersAPI(ctx, client, repo, subject)
		if err != nil {
			return err
		}
		if supported {
			return nil
		}
	}

	// "The value of the artifactType MUST be set to the artifactType value in
	// the pushed manifest, if present. If the artifactType is empty or missing
	// in a pushed image manifest, the value of artifactType MUST be set to the
	// config descriptor mediaType value. All annotations from the pushed
	// manifest MUST be copied to this image index descriptor."
	descriptor := v1.Descriptor{
		MediaType:    mediaType,
		Digest:       digest,
		Size:         size,
		ArtifactType: manifest.ArtifactType,
		Annotations:  manifest.Annotations,
	}
	if descriptor.ArtifactType == "" && manifest.Config != nil {
		descriptor.ArtifactType = manifest.Config.MediaType
	}
	if err := w.addToTagSchema(ctx, client, repo, subject, descriptor); err != nil {
		return fmt.Errorf("listing %s as a referrer of %s: %w", digest, subject, err)
	}
	return nil
}

// writeDependencies uploads what the manifest t refers to: the config and
// layers of an image, the children of an index. Up to w.jobs at once.
func (w *Writer) writeDependencies(ctx context.Context, repo name.Repository, t remote.Taggable) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.jobs)
	switch t := t.(type) {
	case v1.Image:
		config, err := partial.ConfigLayer(t)
		if err != nil {
			return fmt.Errorf("reading the config: %w", err)
		}
		layers, err := t.Layers()
		if err != nil {
			return fmt.Errorf("reading the layers: %w", err)
		}
		g.Go(func() error {
			if err := w.pusher.Upload(gctx, repo, config); err != nil {
				return fmt.Errorf("uploading the config: %w", err)
			}
			return nil
		})
		for _, layer := range layers {
			g.Go(func() error {
				if err := w.pusher.Upload(gctx, repo, layer); err != nil {
					return fmt.Errorf("uploading a layer: %w", err)
				}
				return nil
			})
		}
	case v1.ImageIndex:
		index, err := t.IndexManifest()
		if err != nil {
			return fmt.Errorf("reading the index: %w", err)
		}
		// Every child is resolved before any is pushed, so a bad one doesn't
		// return while pushes of the others are still running.
		children := make([]remote.Taggable, len(index.Manifests))
		for i, child := range index.Manifests {
			switch {
			case child.MediaType.IsIndex():
				children[i], err = t.ImageIndex(child.Digest)
			case child.MediaType.IsImage():
				children[i], err = t.Image(child.Digest)
			default:
				return fmt.Errorf("child %s has the unsupported media type %s", child.Digest, child.MediaType)
			}
			if err != nil {
				return fmt.Errorf("reading the child %s: %w", child.Digest, err)
			}
		}
		for i, child := range index.Manifests {
			g.Go(func() error {
				if err := w.pusher.Push(gctx, repo.Digest(child.Digest.String()), children[i]); err != nil {
					return fmt.Errorf("pushing the child %s: %w", child.Digest, err)
				}
				return nil
			})
		}
	}
	return g.Wait()
}

// putManifest writes a manifest and returns the subject the registry reports
// it processed, if any.
func (w *Writer) putManifest(ctx context.Context, client *http.Client, repo name.Repository, identifier string, mediaType types.MediaType, raw []byte) (string, error) {
	resp, err := w.do(ctx, client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, manifestURL(repo, identifier), bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", string(mediaType))
		return req, nil
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if err := transport.CheckError(resp, http.StatusOK, http.StatusCreated, http.StatusAccepted); err != nil {
		return "", err
	}
	return resp.Header.Get(headerSubject), nil
}

// referrersAPI reports whether the registry's referrers API answers for repo.
// A registry with the API must answer for any digest, so asking for subject
// tells. The answer is remembered for the repository.
func (w *Writer) referrersAPI(ctx context.Context, client *http.Client, repo name.Repository, subject string) (bool, error) {
	key := repo.String()
	w.mu.Lock()
	supported, known := w.api[key]
	w.mu.Unlock()
	if known {
		return supported, nil
	}

	u := (&url.URL{Scheme: repo.Scheme(), Host: repo.RegistryStr(), Path: fmt.Sprintf("/v2/%s/referrers/%s", repo.RepositoryStr(), subject)}).String()
	resp, err := w.do(ctx, client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", mediaTypeIndex)
		return req, nil
	})
	if err != nil {
		return false, fmt.Errorf("checking for the referrers API of %s: %w", repo, err)
	}
	resp.Body.Close()
	supported = resp.StatusCode == http.StatusOK
	if !definitiveAPIAnswer(resp.StatusCode) {
		// A transient failure, or one of authorization: falls back to the tag
		// schema for this referrer, but asks again for the next one.
		return false, nil
	}

	w.mu.Lock()
	w.api[key] = supported
	w.mu.Unlock()
	return supported, nil
}

// definitiveAPIAnswer reports whether a status code returned for the referrers
// API tells whether the registry supports it: a listing, or a route the
// registry doesn't know. Anything else (throttling, server errors, missing
// authorization) may answer differently next time.
func definitiveAPIAnswer(status int) bool {
	switch status {
	case http.StatusOK, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusBadRequest:
		return true
	}
	return false
}

// addToTagSchema makes sure the referrers tag of subject lists descriptor.
func (w *Writer) addToTagSchema(ctx context.Context, client *http.Client, repo name.Repository, subject string, descriptor v1.Descriptor) error {
	tag, err := TagSchemaTag(subject)
	if err != nil {
		return err
	}
	// rejected is the version of the tag (see tagVersion) that a conditional
	// write was last rejected for. Reading the same version again means it
	// hadn't changed: the registry doesn't evaluate the condition the way it
	// should, so the next write goes without (and is checked by reading the tag
	// back).
	rejected := ""
	delay := tagSchemaBaseDelay
	for attempt := 0; ; attempt++ {
		index, version, err := w.getTagIndex(ctx, client, repo, tag)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(index.Manifests, func(d v1.Descriptor) bool { return d.Digest == descriptor.Digest }) {
			return nil
		}
		if attempt == maxTagSchemaAttempts {
			return fmt.Errorf("the referrers tag %s still doesn't list it after %d attempts", tag, attempt)
		}

		condition := version
		if condition == rejected {
			condition = unconditional
		}
		// "Append a descriptor for the pushed manifest to the manifests in the
		// referrers list."
		index.Manifests = append(index.Manifests, descriptor)
		written, err := w.putTagIndex(ctx, client, repo, tag, index, condition)
		if err != nil {
			return err
		}
		if written {
			// Read it back: another writer may have replaced it.
			continue
		}
		rejected = condition
		if err := sleep(ctx, delay); err != nil {
			return err
		}
		delay *= 2
	}
}

// tagVersion is the version of a referrers tag a write is conditional on: the
// ETag of the index it was read as, absent if there was none, or unconditional
// if the registry didn't send an ETag (or a weak one, which never matches the
// strong comparison If-Match uses).
type tagVersion = string

const (
	absent        tagVersion = "\x00absent"
	unconditional tagVersion = ""
)

// getTagIndex reads the referrers tag, and the version to write it back on. A
// missing tag is an empty index.
func (w *Writer) getTagIndex(ctx context.Context, client *http.Client, repo name.Repository, tag string) (*v1.IndexManifest, tagVersion, error) {
	resp, err := w.do(ctx, client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL(repo, tag), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", mediaTypeIndex)
		return req, nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("reading the referrers tag %s: %w", tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// "If the tag returns a 404, the client MUST begin with an empty image
		// index."
		return &v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{}}, absent, nil
	}
	if err := transport.CheckError(resp, http.StatusOK); err != nil {
		return nil, "", fmt.Errorf("reading the referrers tag %s: %w", tag, err)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading the referrers tag %s: %w", tag, err)
	}
	if len(data) > maxIndexSize {
		return nil, "", fmt.Errorf("the referrers tag %s is larger than %d bytes", tag, maxIndexSize)
	}
	// "If that pull returns a manifest other than the expected image index, the
	// client SHOULD report a failure and skip the remaining steps."
	var index v1.IndexManifest
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, "", fmt.Errorf("the referrers tag %s is not an image index: %w", tag, err)
	}
	mediaType := string(index.MediaType)
	if mediaType == "" {
		mediaType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	}
	if index.SchemaVersion != 2 || mediaType != mediaTypeIndex {
		return nil, "", fmt.Errorf("the referrers tag %s is a %s manifest, not an image index", tag, mediaType)
	}
	if index.MediaType == "" {
		index.MediaType = types.OCIImageIndex
	}

	etag := resp.Header.Get("ETag")
	if strings.HasPrefix(etag, "W/") {
		etag = unconditional
	}
	return &index, etag, nil
}

// putTagIndex writes the referrers tag, on the condition that it's still at
// version. It reports false if the condition failed.
func (w *Writer) putTagIndex(ctx context.Context, client *http.Client, repo name.Repository, tag string, index *v1.IndexManifest, version tagVersion) (bool, error) {
	raw, err := json.Marshal(index)
	if err != nil {
		return false, err
	}
	resp, err := w.do(ctx, client, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPut, manifestURL(repo, tag), bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", mediaTypeIndex)
		switch version {
		case unconditional:
		case absent:
			req.Header.Set("If-None-Match", "*")
		default:
			req.Header.Set("If-Match", version)
		}
		return req, nil
	})
	if err != nil {
		return false, fmt.Errorf("writing the referrers tag %s: %w", tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return false, nil
	}
	if err := transport.CheckError(resp, http.StatusOK, http.StatusCreated, http.StatusAccepted); err != nil {
		return false, fmt.Errorf("writing the referrers tag %s: %w", tag, err)
	}
	return true, nil
}

// client returns an authenticated client that may push to repo.
func (w *Writer) client(ctx context.Context, repo name.Repository) (*http.Client, error) {
	key := repo.String()
	w.mu.Lock()
	client := w.clients[key]
	w.mu.Unlock()
	if client != nil {
		return client, nil
	}
	client, err := authenticatedClient(ctx, repo, w.options, repo.Scope(transport.PushScope))
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.clients[key] = client
	w.mu.Unlock()
	return client, nil
}

// do sends the request that build returns, and sends it again (built anew, as
// a body can only be read once) after a network error or a response that says
// to try again later, as Options.Backoff says.
func (w *Writer) do(ctx context.Context, client *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	backoff := w.options.Backoff
	if backoff.Steps < 1 {
		backoff = defaultBackoff
	}
	delay := backoff.Duration
	for attempt := 1; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		retry := false
		switch {
		case err != nil:
			retry = ctx.Err() == nil
		case retryable(resp.StatusCode):
			retry = true
		}
		if !retry || attempt >= backoff.Steps {
			return resp, err
		}
		if resp != nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
		wait := delay
		if backoff.Jitter > 0 {
			wait += time.Duration(rand.Float64() * backoff.Jitter * float64(delay))
		}
		if err := sleep(ctx, wait); err != nil {
			return nil, err
		}
		if backoff.Factor > 0 {
			delay = time.Duration(float64(delay) * backoff.Factor)
		}
		if backoff.Cap > 0 && delay > backoff.Cap {
			delay = backoff.Cap
		}
	}
}

// retryable reports whether a response says to try again later.
func retryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func manifestURL(repo name.Repository, identifier string) string {
	return (&url.URL{Scheme: repo.Scheme(), Host: repo.RegistryStr(), Path: fmt.Sprintf("/v2/%s/manifests/%s", repo.RepositoryStr(), identifier)}).String()
}
