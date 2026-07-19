package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// manifestAccept lists the manifest media types the asserter is willing to
// receive when resolving a reference.
var manifestAccept = strings.Join([]string{
	string(types.OCIImageIndex),
	string(types.OCIManifestSchema1),
	string(types.DockerManifestList),
	string(types.DockerManifestSchema2),
}, ", ")

// registryClient talks plain HTTP to a localhost OCI registry. The integration
// test registry is unauthenticated and served over HTTP, so a bare net/http
// client is both sufficient and maximally transparent.
type registryClient struct {
	base string // e.g. "http://localhost:42133"
	http *http.Client
}

func newRegistryClient(hostPort string) *registryClient {
	return &registryClient{
		base: "http://" + hostPort,
		http: &http.Client{Timeout: 60 * time.Second},
	}
}

// fetchedManifest is a manifest resolved from the registry.
type fetchedManifest struct {
	mediaType types.MediaType
	digest    string // "sha256:..."
	body      []byte
}

func (m fetchedManifest) isIndex() bool { return m.mediaType.IsIndex() }

func (m fetchedManifest) asManifest() (*v1.Manifest, error) {
	var out v1.Manifest
	if err := json.Unmarshal(m.body, &out); err != nil {
		return nil, fmt.Errorf("parsing image manifest %s: %w", m.digest, err)
	}
	return &out, nil
}

func (m fetchedManifest) asIndex() (*v1.IndexManifest, error) {
	var out v1.IndexManifest
	if err := json.Unmarshal(m.body, &out); err != nil {
		return nil, fmt.Errorf("parsing index manifest %s: %w", m.digest, err)
	}
	return &out, nil
}

// getManifest resolves repo:ref (ref is a tag or a "sha256:..." digest).
func (c *registryClient) getManifest(repo, ref string) (*fetchedManifest, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", c.base, repo, ref)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", manifestAccept)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	mt := types.MediaType(resp.Header.Get("Content-Type"))
	if mt == "" {
		// Fall back to the mediaType embedded in the document.
		var probe struct {
			MediaType types.MediaType `json:"mediaType"`
		}
		_ = json.Unmarshal(body, &probe)
		mt = probe.MediaType
	}
	return &fetchedManifest{mediaType: mt, digest: digest, body: body}, nil
}

// headDigest returns the digest a reference resolves to without downloading the
// manifest body.
func (c *registryClient) headDigest(repo, ref string) (string, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", c.base, repo, ref)
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", manifestAccept)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("HEAD %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HEAD %s: status %d", url, resp.StatusCode)
	}
	if d := resp.Header.Get("Docker-Content-Digest"); d != "" {
		return d, nil
	}
	// Fall back to a GET when the registry omits the digest header.
	m, err := c.getManifest(repo, ref)
	if err != nil {
		return "", err
	}
	return m.digest, nil
}

// blobExists reports whether a blob is present in the repository.
func (c *registryClient) blobExists(repo, digest string) (bool, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", c.base, repo, digest)
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("HEAD %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK, nil
}

// referrers returns the referrers index for a subject digest.
func (c *registryClient) referrers(repo, digest string) (*v1.IndexManifest, error) {
	url := fmt.Sprintf("%s/v2/%s/referrers/%s", c.base, repo, digest)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", string(types.OCIImageIndex))
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var idx v1.IndexManifest
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("parsing referrers index for %s: %w", digest, err)
	}
	return &idx, nil
}

// effectiveArtifactType returns a referrer descriptor's artifactType: the
// descriptor's own value when present, otherwise the referring manifest's
// artifactType (or, for an image manifest, its config media type). Registries do
// not always propagate artifactType into the referrers index, so fall back to
// the manifest itself.
func (c *registryClient) effectiveArtifactType(repo string, d v1.Descriptor) string {
	if d.ArtifactType != "" {
		return d.ArtifactType
	}
	m, err := c.getManifest(repo, d.Digest.String())
	if err != nil {
		return ""
	}
	if m.isIndex() {
		if idx, err := m.asIndex(); err == nil {
			return idx.ArtifactType
		}
		return ""
	}
	man, err := m.asManifest()
	if err != nil {
		return ""
	}
	if man.ArtifactType != "" {
		return man.ArtifactType
	}
	return string(man.Config.MediaType)
}

// configLabels fetches and parses the config blob of a single-arch manifest and
// returns its image labels.
func (c *registryClient) configLabels(repo string, config v1.Descriptor) (map[string]string, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", c.base, repo, config.Digest.String())
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	var cfg v1.ConfigFile
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", config.Digest, err)
	}
	return cfg.Config.Labels, nil
}
