package credential

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/malt3/go-containerregistry/pkg/authn"
)

// Helper is the interface for a credential helper.
type Helper interface {
	Get(ctx context.Context, uri string) (headers map[string][]string, expiresAt time.Time, err error)
}

// Options configures the behavior of a credential helper.
type Options struct {
	// CaptureStderr captures the credential helper's stderr output instead of
	// forwarding it to os.Stderr. When set, stderr content is included in any
	// error returned by Get.
	CaptureStderr bool
}

type externalCredentialHelper struct {
	helperBinary  string
	captureStderr bool
	cache         map[string]cacheEntry
	mux           sync.RWMutex
}

func New(credentialHelperBinary string, opts *Options) Helper {
	workingDirectory := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if workingDirectory != "" {
		credentialHelperBinary = strings.Replace(credentialHelperBinary, "%workspace%", workingDirectory, 1)
	}
	var captureStderr bool
	if opts != nil {
		captureStderr = opts.CaptureStderr
	}
	return &externalCredentialHelper{
		helperBinary:  credentialHelperBinary,
		captureStderr: captureStderr,
		cache:         make(map[string]cacheEntry),
	}
}

func (e *externalCredentialHelper) Get(ctx context.Context, uri string) (headers map[string][]string, expiresAt time.Time, err error) {
	if headers, ok := e.getFromCache(uri); ok {
		return headers, expiresAt, nil
	}
	cmd := exec.CommandContext(ctx, e.helperBinary, "get")
	stdin, err := json.Marshal(externalRequest{URI: uri})
	if err != nil {
		return nil, time.Time{}, err
	}
	var stderrBuf bytes.Buffer
	if e.captureStderr {
		cmd.Stderr = &stderrBuf
	} else {
		cmd.Stderr = os.Stderr
	}
	cmd.Stdin = bytes.NewReader(stdin)
	stdout, err := cmd.Output()
	if err != nil {
		if e.captureStderr {
			if stderrContent := stderrBuf.String(); stderrContent != "" {
				return nil, time.Time{}, fmt.Errorf("%w\ncredential helper stderr:\n%s", err, stderrContent)
			}
		}
		return nil, time.Time{}, err
	}
	var resp externalResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, time.Time{}, err
	}

	if resp.Expires != "" {
		expiresAt, err = time.Parse(time.RFC3339, resp.Expires)
		if err != nil {
			return nil, time.Time{}, err
		}
	}
	e.putToCache(uri, resp.Headers, expiresAt)
	return resp.Headers, expiresAt, nil
}

func (e *externalCredentialHelper) getFromCache(uri string) (headers map[string][]string, ok bool) {
	e.mux.RLock()
	defer e.mux.RUnlock()
	entry, ok := e.cache[uri]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.headers, true
}

func (e *externalCredentialHelper) putToCache(uri string, headers map[string][]string, expiresAt time.Time) {
	e.mux.Lock()
	defer e.mux.Unlock()
	if expiresAt.IsZero() {
		// TODO: make this configurable
		expiresAt = time.Now().Add(5 * time.Minute)
	}
	e.cache[uri] = cacheEntry{
		headers:   headers,
		expiresAt: expiresAt,
	}
}

type nopHelper struct{}

func NopHelper() Helper {
	return nopHelper{}
}

func (nopHelper) Get(ctx context.Context, uri string) (map[string][]string, time.Time, error) {
	return nil, time.Time{}, nil
}

type AuthenticatingRoundTripper struct {
	helper Helper
}

func RoundTripper(helper Helper) *AuthenticatingRoundTripper {
	return &AuthenticatingRoundTripper{
		helper: helper,
	}
}

func (a *AuthenticatingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	headers, _, err := a.helper.Get(req.Context(), req.URL.String())
	if err != nil {
		return nil, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

type externalRequest struct {
	URI string `json:"uri"`
}

type externalResponse struct {
	Expires string              `json:"expires,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
}

type cacheEntry struct {
	headers   map[string][]string
	expiresAt time.Time
}

var _ http.RoundTripper = &AuthenticatingRoundTripper{}

// ContainerRegistryHelper conforms to `go-containerregistry#authn.Helper`
type containerRegistryHelper struct {
	helper Helper
}

func ContainerRegistryHelper(helper Helper) *containerRegistryHelper {
	return &containerRegistryHelper{
		helper: helper,
	}
}

func (c *containerRegistryHelper) Get(serverURL string) (string, string, error) {
	cfg, err := c.authConfig(context.Background(), serverURL)
	if err != nil {
		return "", "", err
	}
	if cfg.RegistryToken != "" {
		// WARNING: Docker helper pairs cannot represent RegistryToken directly.
		// This legacy method can only approximate Bearer as an identity token;
		// registry auth must use ContainerRegistryKeychain to preserve access-token semantics.
		return "<token>", cfg.RegistryToken, nil
	}
	return cfg.Username, cfg.Password, nil
}

func (c *containerRegistryHelper) authConfig(ctx context.Context, serverURL string) (authn.AuthConfig, error) {
	headers, _, err := c.helper.Get(ctx, serverURL)
	if err != nil {
		return authn.AuthConfig{}, err
	} else if headers == nil {
		return authn.AuthConfig{}, errors.New("no HTTP headers found")
	}

	values, ok := headers["Authorization"]
	if !ok {
		return authn.AuthConfig{}, errors.New("no `Authorization` header")
	}

	for _, header := range values {
		kind, value, found := strings.Cut(header, " ")
		if !found {
			return authn.AuthConfig{}, fmt.Errorf("no authorization scheme: %s", header)
		} else if strings.EqualFold(kind, "Basic") {
			decoded, err := base64.StdEncoding.DecodeString(value)
			if err != nil {
				return authn.AuthConfig{}, fmt.Errorf("decode authorisation header: %s: %w", header, err)
			}
			username, password, found := strings.Cut(string(decoded), ":")
			if !found {
				return authn.AuthConfig{}, fmt.Errorf("no semi-colon in basic auth: %s", decoded)
			}
			return authn.AuthConfig{Username: username, Password: password}, nil
		} else if strings.EqualFold(kind, "Bearer") {
			// Bazel credential helpers emit ready-to-send HTTP headers. Treat Bearer as
			// a registry access token; using IdentityToken would make go-containerregistry
			// try an OAuth refresh-token exchange instead.
			return authn.AuthConfig{RegistryToken: value}, nil
		} else {
			return authn.AuthConfig{}, fmt.Errorf("unknown authorization scheme: %s", header)
		}
	}

	return authn.AuthConfig{}, fmt.Errorf("no `Authorization` headers")
}

// ContainerRegistryKeychain adapts Bazel credential-helper HTTP headers to
// go-containerregistry authentication without losing Bearer token semantics.
func ContainerRegistryKeychain(helper Helper) authn.Keychain {
	return &containerRegistryKeychain{
		helper: ContainerRegistryHelper(helper),
	}
}

type containerRegistryKeychain struct {
	helper *containerRegistryHelper
}

func (c *containerRegistryKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return c.ResolveContext(context.Background(), target)
}

func (c *containerRegistryKeychain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	cfg, err := c.helper.authConfig(ctx, target.RegistryStr())
	if err != nil {
		return authn.Anonymous, nil
	}
	return authn.FromConfig(cfg), nil
}
