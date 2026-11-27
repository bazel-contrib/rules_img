// Package referrers lists the referrers of a manifest, and pushes them,
// following the OCI distribution spec: the referrers API, falling back to the
// referrers tag schema on registries that don't support it.
//
// See https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers
// and https://github.com/opencontainers/distribution-spec/blob/main/spec.md#referrers-tag-schema.
package referrers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"golang.org/x/sync/errgroup"
)

const (
	mediaTypeIndex = "application/vnd.oci.image.index.v1+json"
	// maxPages bounds how many pages of a paginated referrers response are
	// followed, so a registry that keeps handing out "next" links can't loop
	// the client forever.
	maxPages = 1000
	// maxIndexSize bounds a single page of a referrers response.
	maxIndexSize = 4 << 20
	// inspectJobs bounds how many manifests listed by a referrers tag are
	// fetched at once.
	inspectJobs = 8
)

// Options configures how a registry is accessed.
type Options struct {
	// Transport is the base transport. Authentication is layered on top of it.
	Transport http.RoundTripper
	// Keychain resolves the credentials for the repository.
	Keychain authn.Keychain
	// Remote are the go-containerregistry options for fetching manifests (the
	// referrers tag, and listed manifests). They should use the same transport
	// and keychain.
	Remote []remote.Option
	// Backoff says how often, and after how long, a Writer sends a request
	// again after a network error or a response that says to try again later.
	// Unset, it tries three times.
	Backoff remote.Backoff
	// Index is the image index (raw) whose entries BuildKitAttestationType
	// searches: the index the subjects of Discover belong to. Unset, they
	// have no BuildKit attestations.
	Index []byte
}

// Source tells where a listing came from.
type Source string

const (
	// SourceAPI is the registry's referrers API, which computes the listing
	// from the manifests it stores.
	SourceAPI Source = "referrers-api"
	// SourceTagSchema is the referrers tag schema: an index that clients
	// maintain under a tag derived from the subject's digest.
	SourceTagSchema Source = "tag-schema"
)

// List returns the descriptors that refer to subject in repo, and where the
// listing came from. If exactly one artifact type is given, it is passed to the
// referrers API as a filter, which a registry may or may not apply; callers
// filter the result themselves either way.
//
// Following the spec, the client falls back to the referrers tag schema only if
// the referrers API answers 404 Not Found, and assumes there are no referrers if
// the tag then doesn't return a valid image index.
func List(ctx context.Context, repo name.Repository, subject string, artifactTypes []string, options Options) ([]v1.Descriptor, Source, error) {
	if _, err := v1.NewHash(subject); err != nil {
		return nil, "", fmt.Errorf("invalid subject digest %q: %w", subject, err)
	}
	client, err := authenticatedClient(ctx, repo, options)
	if err != nil {
		return nil, "", err
	}

	query := url.Values{}
	if len(artifactTypes) == 1 {
		query.Set("artifactType", artifactTypes[0])
	}
	next := &url.URL{
		Scheme:   repo.Scheme(),
		Host:     repo.RegistryStr(),
		Path:     fmt.Sprintf("/v2/%s/referrers/%s", repo.RepositoryStr(), subject),
		RawQuery: query.Encode(),
	}
	var descriptors []v1.Descriptor
	for page := 0; next != nil; page++ {
		if page == maxPages {
			return nil, "", fmt.Errorf("listing referrers of %s: more than %d pages", subject, maxPages)
		}
		manifests, link, notFound, err := fetchPage(ctx, client, next)
		if err != nil {
			return nil, "", fmt.Errorf("listing referrers of %s: %w", subject, err)
		}
		if notFound {
			if page > 0 {
				return nil, "", fmt.Errorf("listing referrers of %s: page %s not found", subject, next)
			}
			// "If the referrers API returns a 404, the client MUST fallback to
			// pulling the referrers tag schema."
			descriptors, err := listTagSchema(ctx, repo, subject, options)
			return descriptors, SourceTagSchema, err
		}
		descriptors = append(descriptors, manifests...)
		next = link
	}
	return descriptors, SourceAPI, nil
}

// fetchPage gets one page of a referrers API response. It returns the
// descriptors, the URL of the next page (if any), and whether the endpoint
// answered 404 Not Found.
func fetchPage(ctx context.Context, client *http.Client, u *url.URL) ([]v1.Descriptor, *url.URL, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, false, err
	}
	req.Header.Set("Accept", mediaTypeIndex)
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil, true, nil
	}
	if err := transport.CheckError(resp, http.StatusOK); err != nil {
		return nil, nil, false, err
	}
	// "The Content-Type header MUST be set to application/vnd.oci.image.index.v1+json."
	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mediaType != mediaTypeIndex {
		return nil, nil, false, fmt.Errorf("referrers API answered with Content-Type %q instead of %s", resp.Header.Get("Content-Type"), mediaTypeIndex)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexSize+1))
	if err != nil {
		return nil, nil, false, err
	}
	if len(body) > maxIndexSize {
		return nil, nil, false, fmt.Errorf("referrers response exceeds %d bytes", maxIndexSize)
	}
	var index v1.IndexManifest
	if err := json.Unmarshal(body, &index); err != nil {
		return nil, nil, false, fmt.Errorf("parsing referrers response: %w", err)
	}
	next, err := nextLink(u, resp.Header.Values("Link"))
	if err != nil {
		return nil, nil, false, err
	}
	return index.Manifests, next, false, nil
}

var linkPattern = regexp.MustCompile(`^\s*<([^>]*)>(.*)$`)

// nextLink returns the target of the RFC 5988 Link header with rel="next",
// resolved against the URL of the current page, or nil if there is none.
func nextLink(current *url.URL, headers []string) (*url.URL, error) {
	for _, header := range headers {
		for _, link := range splitLinks(header) {
			match := linkPattern.FindStringSubmatch(link)
			if match == nil {
				continue
			}
			isNext := false
			for _, param := range strings.Split(match[2], ";") {
				key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "rel") {
					continue
				}
				for _, rel := range strings.Fields(strings.Trim(strings.TrimSpace(value), `"`)) {
					if strings.EqualFold(rel, "next") {
						isNext = true
					}
				}
			}
			if !isNext {
				continue
			}
			target, err := url.Parse(match[1])
			if err != nil {
				return nil, fmt.Errorf("parsing Link header %q: %w", header, err)
			}
			return current.ResolveReference(target), nil
		}
	}
	return nil, nil
}

// splitLinks splits a Link header into its comma-separated links, ignoring
// commas inside the <...> target.
func splitLinks(header string) []string {
	var links []string
	depth := 0
	start := 0
	for i, r := range header {
		switch r {
		case '<':
			depth++
		case '>':
			depth--
		case ',':
			if depth == 0 {
				links = append(links, header[start:i])
				start = i + 1
			}
		}
	}
	return append(links, header[start:])
}

// TagSchemaTag returns the referrers tag of a digest: the algorithm truncated
// to 32 characters, a "-", and the encoded section truncated to 64 characters,
// with any character not allowed in a tag replaced by "-".
func TagSchemaTag(digest string) (string, error) {
	algorithm, encoded, ok := strings.Cut(digest, ":")
	if !ok || algorithm == "" || encoded == "" {
		return "", fmt.Errorf("invalid digest %q", digest)
	}
	tag := truncate(algorithm, 32) + "-" + truncate(encoded, 64)
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == '-' {
			return r
		}
		return '-'
	}, tag), nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// listTagSchema reads the referrers tag of subject. A missing tag, or one that
// is not a valid image index, means there are no referrers.
func listTagSchema(ctx context.Context, repo name.Repository, subject string, options Options) ([]v1.Descriptor, error) {
	tag, err := TagSchemaTag(subject)
	if err != nil {
		return nil, err
	}
	descriptor, err := remote.Get(repo.Tag(tag), append(slices.Clone(options.Remote), remote.WithContext(ctx))...)
	var terr *transport.Error
	if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("pulling referrers tag %s: %w", tag, err)
	}
	if descriptor.MediaType != mediaTypeIndex {
		return nil, nil
	}
	var index v1.IndexManifest
	if err := json.Unmarshal(descriptor.Manifest, &index); err != nil {
		return nil, nil
	}
	return index.Manifests, nil
}

func authenticatedClient(ctx context.Context, repo name.Repository, options Options, scopes ...string) (*http.Client, error) {
	base := options.Transport
	if base == nil {
		base = remote.DefaultTransport
	}
	auth := authn.Anonymous
	if options.Keychain != nil {
		var err error
		auth, err = authn.Resolve(ctx, options.Keychain, repo)
		if err != nil {
			return nil, fmt.Errorf("resolving credentials for %s: %w", repo, err)
		}
	}
	if len(scopes) == 0 {
		scopes = []string{repo.Scope(transport.PullScope)}
	}
	rt, err := transport.NewWithContext(ctx, repo.Registry, auth, base, scopes)
	if err != nil {
		return nil, fmt.Errorf("authenticating to %s: %w", repo.RegistryStr(), err)
	}
	return &http.Client{Transport: rt}, nil
}

// AnyArtifactType, as a requested artifact type, selects the referrers of any
// artifact type.
const AnyArtifactType = "*"

// Discover lists the referrers of the manifest subject in repo whose artifact
// type is one of artifactTypes. AnyArtifactType matches every referrer.
//
// The result maps every requested artifact type to the sorted digests of its
// referrers, so a type without referrers is recorded as such (an empty list).
// A referrer is listed under every requested type it matches: its own and
// AnyArtifactType.
//
// A listing from the referrers API is trusted: the registry computes each
// descriptor's artifactType and the subject relation from the stored manifests.
// A referrers tag is not: clients maintain it, and in practice its descriptors
// may lack an artifactType or carry a different one than the manifest (cosign
// records the config's media type), and the tag may not even be a referrers
// index (a tag that happens to be named after the digest). So every manifest it
// lists is fetched, and kept only if its own artifact type (artifactType, or
// the config's media type) is requested and its subject is the manifest.
//
// The pseudo artifact types made by CosignTagType find cosign's tag-scheme
// artifacts instead (see discoverCosign), which aren't referrers in the OCI
// sense: they're found by a tag named after subject, and only their signed
// payloads tie them to it. AnyArtifactType doesn't select them: request them
// next to it. So does BuildKitAttestationType, which finds the BuildKit
// attestation manifests that options.Index lists for subject (see
// BuildKitAttestations): no request is made for those.
func Discover(ctx context.Context, repo name.Repository, subject string, artifactTypes []string, options Options) (map[string][]string, error) {
	found := make(map[string][]string, len(artifactTypes))
	var ociTypes []string
	for _, artifactType := range artifactTypes {
		found[artifactType] = []string{}
		if artifactType == BuildKitAttestationType {
			attestations, err := BuildKitAttestations(options.Index, subject)
			if err != nil {
				return nil, err
			}
			found[artifactType] = attestations
			continue
		}
		suffix, ok := cosignTagSuffix(artifactType)
		if !ok {
			ociTypes = append(ociTypes, artifactType)
			continue
		}
		digest, err := discoverCosign(ctx, repo, subject, suffix, options)
		if err != nil {
			return nil, err
		}
		if digest != "" {
			found[artifactType] = []string{digest}
		}
	}
	if len(ociTypes) == 0 {
		return found, nil
	}
	filter := ociTypes
	if slices.Contains(ociTypes, AnyArtifactType) {
		// Nothing to filter on: the registry must list them all.
		filter = nil
	}
	descriptors, source, err := List(ctx, repo, subject, filter, options)
	if err != nil {
		return nil, err
	}
	add := func(key, digest string) {
		if digests, requested := found[key]; requested && !slices.Contains(digests, digest) {
			found[key] = append(digests, digest)
		}
	}
	// Only image manifests can be referrers of a pulled image: the others (an
	// index, such as an ORAS index artifact) are skipped, as if not listed.
	descriptors = slices.DeleteFunc(descriptors, func(d v1.Descriptor) bool { return !isImageManifest(d.MediaType) })
	listedTypes := make([]string, len(descriptors))
	skip := make([]bool, len(descriptors))
	for i, descriptor := range descriptors {
		listedTypes[i] = descriptor.ArtifactType
	}
	if source == SourceTagSchema {
		// The tag may list many manifests: they're inspected concurrently.
		referred := make([]string, len(descriptors))
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(inspectJobs)
		for i, descriptor := range descriptors {
			g.Go(func() error {
				var err error
				listedTypes[i], referred[i], err = inspect(gctx, repo, descriptor.Digest.String(), options)
				if err != nil {
					return fmt.Errorf("inspecting referrer %s of %s: %w", descriptor.Digest, subject, err)
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
		for i := range descriptors {
			skip[i] = referred[i] != subject
		}
	}
	for i, descriptor := range descriptors {
		if skip[i] {
			continue
		}
		if listedTypes[i] != AnyArtifactType {
			add(listedTypes[i], descriptor.Digest.String())
		}
		add(AnyArtifactType, descriptor.Digest.String())
	}
	for _, digests := range found {
		slices.Sort(digests)
	}
	return found, nil
}

// inspect fetches a manifest and returns its artifact type and the digest of
// its subject (empty if it has none).
func inspect(ctx context.Context, repo name.Repository, digest string, options Options) (string, string, error) {
	descriptor, err := remote.Get(repo.Digest(digest), append(slices.Clone(options.Remote), remote.WithContext(ctx))...)
	if err != nil {
		return "", "", err
	}
	var manifest struct {
		ArtifactType string `json:"artifactType"`
		Config       struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
		Subject *struct {
			Digest string `json:"digest"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(descriptor.Manifest, &manifest); err != nil {
		return "", "", err
	}
	artifactType := manifest.ArtifactType
	if artifactType == "" {
		artifactType = manifest.Config.MediaType
	}
	if manifest.Subject == nil {
		return artifactType, "", nil
	}
	return artifactType, manifest.Subject.Digest, nil
}

// isImageManifest reports whether a listed referrer is an image manifest. A
// descriptor without a media type is assumed to be one.
func isImageManifest(mediaType types.MediaType) bool {
	return mediaType == "" || mediaType.IsImage()
}

// Flatten returns the sorted, deduplicated digests of a Discover result.
func Flatten(found map[string][]string) []string {
	var all []string
	for _, digests := range found {
		for _, digest := range digests {
			if !slices.Contains(all, digest) {
				all = append(all, digest)
			}
		}
	}
	slices.Sort(all)
	return all
}
