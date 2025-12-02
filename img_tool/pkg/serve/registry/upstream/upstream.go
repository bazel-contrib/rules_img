package upstream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	registry "github.com/google/go-containerregistry/pkg/registry"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	reg "github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
)

type UpstreamBlobHandler struct {
	registryURL string
}

func New(registryURL string) *UpstreamBlobHandler {
	return &UpstreamBlobHandler{
		registryURL: registryURL,
	}
}

func (h *UpstreamBlobHandler) Get(ctx context.Context, repo string, hash registryv1.Hash) (io.ReadCloser, error) {
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", h.registryURL, repo, hash.String()))
	if err != nil {
		return nil, fmt.Errorf("creating blob reference: %w", err)
	}
	transport := &redirectHandler{
		underlying: remote.DefaultTransport,
	}
	layer, err := remote.Layer(ref, reg.WithAuthFromMultiKeychain(), remote.WithTransport(transport))
	if err != nil {
		return nil, fmt.Errorf("getting layer: %w", err)
	}
	reader, err := layer.Compressed()
	if err != nil {
		return nil, fmt.Errorf("getting layer: %w", err)
	}
	if transport.redirectTarget != "" {
		reader.Close()
		return nil, registry.RedirectError{
			Location: transport.redirectTarget,
			Code:     http.StatusFound,
		}
	}
	return reader, nil
}

func (h *UpstreamBlobHandler) Stat(ctx context.Context, repo string, hash registryv1.Hash) (int64, error) {
	ref, err := name.NewDigest(fmt.Sprintf("%s/%s@%s", h.registryURL, repo, hash.String()))
	if err != nil {
		return 0, fmt.Errorf("creating blob reference: %w", err)
	}
	transport := &redirectHandler{
		hash:       hash,
		underlying: remote.DefaultTransport,
	}
	layer, err := remote.Layer(ref, reg.WithAuthFromMultiKeychain(), remote.WithTransport(transport))
	if err != nil {
		return 0, fmt.Errorf("getting layer: %w", err)
	}
	size, err := layer.Size()
	if err != nil {
		return 0, fmt.Errorf("getting layer size: %w", err)
	}

	if transport.redirectTarget != "" {
		return size, registry.RedirectError{
			Location: transport.redirectTarget,
			Code:     http.StatusFound,
		}
	}
	return size, nil
}

type redirectHandler struct {
	hash           registryv1.Hash
	underlying     http.RoundTripper
	redirectTarget string
}

func (h *redirectHandler) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := h.underlying.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	// we are only interested in response "Location" headers (redirects) with:
	// request path: /v2/<name>/blobs/<digest>
	if !strings.HasPrefix(req.URL.Path, "/v2/") || !strings.HasSuffix(req.URL.Path, fmt.Sprintf("/blobs/%s", h.hash.String())) {
		return resp, nil
	}

	location, err := resp.Location()
	if err == nil && location.String() != req.URL.String() {
		h.redirectTarget = location.String()
	}

	return resp, nil
}
