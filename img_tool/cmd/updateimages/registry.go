package updateimages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	imgreferrers "github.com/bazel-contrib/rules_img/img_tool/pkg/referrers"
)

// registryClient reads from registries. Every operation takes the sources of
// an image and tries them in order, like the images extension does.
type registryClient struct {
	remote    []remote.Option
	referrers imgreferrers.Options
}

func (s source) repository() (name.Repository, error) {
	return name.NewRepository(s.String(), nameOptions()...)
}

// firstSource runs op on each source until one succeeds.
func firstSource[T any](sources []source, op func(source, name.Repository) (T, error)) (T, source, error) {
	var zero T
	var errs []error
	for _, src := range sources {
		repo, err := src.repository()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src, err))
			continue
		}
		result, err := op(src, repo)
		if err == nil {
			return result, src, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", src, err))
	}
	if len(errs) == 0 {
		return zero, source{}, errors.New("no sources")
	}
	return zero, source{}, errors.Join(errs...)
}

func (c *registryClient) options(ctx context.Context) []remote.Option {
	return append(append([]remote.Option{}, c.remote...), remote.WithContext(ctx))
}

// resolveTag returns the digest that tag points to.
func (c *registryClient) resolveTag(ctx context.Context, sources []source, tag string) (string, source, error) {
	return firstSource(sources, func(_ source, repo name.Repository) (string, error) {
		ref := repo.Tag(tag)
		// HEAD doesn't count against Docker Hub's pull rate limit.
		descriptor, err := remote.Head(ref, c.options(ctx)...)
		if err == nil && descriptor.Digest.Hex != "" {
			return descriptor.Digest.String(), nil
		}
		if isNotFound(err) {
			return "", fmt.Errorf("tag %q not found", tag)
		}
		got, err := remote.Get(ref, c.options(ctx)...)
		if err != nil {
			return "", err
		}
		return got.Digest.String(), nil
	})
}

// listTags returns the tags of the repository.
func (c *registryClient) listTags(ctx context.Context, sources []source) ([]string, source, error) {
	return firstSource(sources, func(_ source, repo name.Repository) ([]string, error) {
		return remote.List(repo, c.options(ctx)...)
	})
}

// manifest is the part of an image manifest or index this tool looks at.
type manifest struct {
	MediaType    string            `json:"mediaType"`
	ArtifactType string            `json:"artifactType"`
	Config       *v1.Descriptor    `json:"config"`
	Layers       []v1.Descriptor   `json:"layers"`
	Manifests    []v1.Descriptor   `json:"manifests"`
	Subject      *v1.Descriptor    `json:"subject"`
	Annotations  map[string]string `json:"annotations"`
}

func (m *manifest) isIndex() bool {
	return m.Manifests != nil || strings.Contains(m.MediaType, "index") || strings.Contains(m.MediaType, "manifest.list")
}

// artifactType is the artifact type of a manifest, as the referrers API
// reports it: artifactType, or else the media type of the config.
func (m *manifest) artifactType() string {
	if m.ArtifactType != "" {
		return m.ArtifactType
	}
	if m.Config != nil {
		return string(m.Config.MediaType)
	}
	return ""
}

type fetched struct {
	raw      []byte
	size     int64
	manifest manifest
}

func (c *registryClient) getManifestFrom(ctx context.Context, repo name.Repository, digest string) (fetched, error) {
	descriptor, err := remote.Get(repo.Digest(digest), c.options(ctx)...)
	if err != nil {
		return fetched{}, err
	}
	var m manifest
	if err := json.Unmarshal(descriptor.Manifest, &m); err != nil {
		return fetched{}, fmt.Errorf("parsing manifest %s: %w", digest, err)
	}
	if m.MediaType == "" {
		m.MediaType = string(descriptor.MediaType)
	}
	return fetched{raw: descriptor.Manifest, size: descriptor.Size, manifest: m}, nil
}

func (c *registryClient) getManifest(ctx context.Context, sources []source, digest string) (fetched, source, error) {
	return firstSource(sources, func(_ source, repo name.Repository) (fetched, error) {
		return c.getManifestFrom(ctx, repo, digest)
	})
}

// subject is a manifest that referrers can apply to: a pulled manifest or
// index, or a child manifest of a pulled index.
type subject struct {
	Digest string `json:"digest"`
	// Platform is the platform of a child manifest, empty for the root.
	Platform string `json:"platform,omitempty"`
	Index    bool   `json:"index,omitempty"`
}

func (s subject) label() string {
	if s.Platform != "" {
		return s.Platform
	}
	if s.Index {
		return "index"
	}
	return "manifest"
}

// tree returns digest and, if it is an index, its children, and the raw index
// (nil if digest is a manifest), whose entries name the BuildKit attestations
// of the children.
func (c *registryClient) tree(ctx context.Context, sources []source, digest string) ([]subject, []byte, error) {
	got, _, err := c.getManifest(ctx, sources, digest)
	if err != nil {
		return nil, nil, err
	}
	subjects := []subject{{Digest: digest, Index: got.manifest.isIndex()}}
	for _, child := range got.manifest.Manifests {
		subjects = append(subjects, subject{Digest: child.Digest.String(), Platform: platformString(child.Platform)})
	}
	var index []byte
	if got.manifest.isIndex() {
		index = got.raw
	}
	return subjects, index, nil
}

func platformString(p *v1.Platform) string {
	if p == nil || p.OS == "" {
		return "unknown"
	}
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func isNotFound(err error) bool {
	var terr *transport.Error
	return errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound
}
