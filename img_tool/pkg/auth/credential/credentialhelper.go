package credential

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credcache"
	"github.com/google/go-containerregistry/pkg/authn"
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
	// DebugLog, if set, receives a line for every run of the helper (cache
	// hits do not run it) and the helper's stderr as it is written, in
	// addition to wherever CaptureStderr sends it.
	DebugLog io.Writer
}

// defaultExpiry is how long a credential is reused when the helper's response
// carries no expiry.
// TODO: make this configurable
const defaultExpiry = 60 * time.Minute

type externalCredentialHelper struct {
	helperBinary  string
	captureStderr bool
	debugLog      io.Writer
	// cache is keyed by the requested URI, which is the only input the helper
	// sees, so concurrent lookups of one URI run the helper once.
	cache *credcache.Cache[map[string][]string]
}

func New(credentialHelperBinary string, opts *Options) Helper {
	if strings.Contains(credentialHelperBinary, "%workspace%") {
		if workingDirectory := os.Getenv("BUILD_WORKSPACE_DIRECTORY"); workingDirectory != "" {
			credentialHelperBinary = strings.ReplaceAll(credentialHelperBinary, "%workspace%", workingDirectory)
		} else {
			fmt.Fprintf(os.Stderr, "warning: credential helper path %q contains %%workspace%% but BUILD_WORKSPACE_DIRECTORY is not set; leaving the placeholder unresolved\n", credentialHelperBinary)
		}
	}
	var captureStderr bool
	var debugLog io.Writer
	if opts != nil {
		captureStderr = opts.CaptureStderr
		debugLog = opts.DebugLog
	}
	return &externalCredentialHelper{
		helperBinary:  credentialHelperBinary,
		captureStderr: captureStderr,
		debugLog:      debugLog,
		cache:         credcache.New[map[string][]string](defaultExpiry),
	}
}

func (e *externalCredentialHelper) Get(ctx context.Context, uri string) (headers map[string][]string, expiresAt time.Time, err error) {
	return e.cache.Get(ctx, uri, func(ctx context.Context) (map[string][]string, time.Time, error) {
		return e.run(ctx, uri)
	})
}

// run invokes the helper binary for uri.
func (e *externalCredentialHelper) run(ctx context.Context, uri string) (headers map[string][]string, expiresAt time.Time, err error) {
	if e.debugLog != nil {
		fmt.Fprintf(e.debugLog, "credential helper %s: running for %s\n", e.helperBinary, uri)
		defer func() {
			switch {
			case err != nil:
				fmt.Fprintf(e.debugLog, "credential helper %s for %s: error: %v\n", e.helperBinary, uri, err)
			case expiresAt.IsZero():
				fmt.Fprintf(e.debugLog, "credential helper %s for %s: got credentials without an expiry\n", e.helperBinary, uri)
			default:
				fmt.Fprintf(e.debugLog, "credential helper %s for %s: got credentials expiring at %s\n", e.helperBinary, uri, expiresAt.Format(time.RFC3339))
			}
		}()
	}
	cmd := exec.CommandContext(ctx, e.helperBinary, "get")
	stdin, err := json.Marshal(externalRequest{URI: uri})
	if err != nil {
		return nil, time.Time{}, err
	}
	var stderrBuf bytes.Buffer
	switch {
	case e.captureStderr && e.debugLog != nil:
		cmd.Stderr = io.MultiWriter(&stderrBuf, e.debugLog)
	case e.captureStderr:
		cmd.Stderr = &stderrBuf
	default:
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
	return resp.Headers, expiresAt, nil
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
