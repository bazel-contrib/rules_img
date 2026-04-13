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
)

// Helper is the interface for a credential helper.
type Helper interface {
	Get(ctx context.Context, uri string) (headers map[string][]string, expiresAt time.Time, err error)
}

type externalCredentialHelper struct {
	helperBinary string
	cache        map[string]cacheEntry
	mux          sync.RWMutex
}

func New(credentialHelperBinary string) Helper {
	workingDirectory := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	if workingDirectory != "" {
		credentialHelperBinary = strings.Replace(credentialHelperBinary, "%workspace%", workingDirectory, 1)
	}
	return &externalCredentialHelper{
		helperBinary: credentialHelperBinary,
		cache:        make(map[string]cacheEntry),
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
	cmd.Stderr = os.Stderr
	cmd.Stdin = bytes.NewReader(stdin)
	stdout, err := cmd.Output()
	if err != nil {
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
	headers, _, err := c.helper.Get(context.Background(), serverURL)
	if err != nil {
		return "", "", err
	} else if headers == nil {
		return "", "", errors.New("no HTTP headers found")
	}

	values, ok := headers["Authorization"]
	if !ok {
		return "", "", errors.New("no `Authorization` header")
	}

	for _, header := range values {
		kind, value, found := strings.Cut(header, " ")
		if !found {
			return "", "", fmt.Errorf("no authorization scheme: %s", header)
		} else if kind == "Basic" {
			decoded, err := base64.StdEncoding.DecodeString(value)
			if err != nil {
				return "", "", fmt.Errorf("decode authorisation header: %s: %w", header, err)
			}
			username, password, found := strings.Cut(string(decoded), ":")
			if !found {
				return "", "", fmt.Errorf("no semi-colon in basic auth: %s", decoded)
			}
			return username, password, nil
		} else if kind == "Bearer" {
			return "<token>", value, nil
		} else {
			return "", "", fmt.Errorf("unknown authorization scheme: %s", header)
		}
	}

	return "", "", fmt.Errorf("no `Authorization` headers")
}
