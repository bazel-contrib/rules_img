package registry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	ecr "github.com/awslabs/amazon-ecr-credential-helper/ecr-login"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sirupsen/logrus"
)

// amazonKeychain authenticates to Amazon ECR registries using the
// amazon-ecr-credential-helper, resolving credentials from the ambient AWS
// configuration (environment, shared config files, or instance/role metadata).
//
// Lookups are memoized per registry (see [cachingHelper]). Each call into the
// ECR helper loads the AWS config from scratch, retrieves the ambient AWS
// credentials (an STS or IMDS round trip), and may call
// ecr:GetAuthorizationToken, even though one token covers the whole registry.
// Uncached, a push to many repositories at once sends a burst of identical
// requests to AWS and gets throttled.
//
// It is built on first use, not at init, so that it sees IMG_AUTH_DEBUG as set
// by then. It is shared by every keychain built in the process, so is its cache.
var amazonKeychain = sync.OnceValue(func() authn.Keychain {
	logger := io.Writer(io.Discard)
	var debugLog io.Writer
	if authDebug() {
		logger, debugLog = authDebugLog, authDebugLog
		// The helper's API client and token cache log through the logrus
		// standard logger, mostly at debug level ("Using cached token",
		// "Got error fetching authorization token. Falling back to cached
		// token.").
		logrus.SetOutput(authDebugLog)
		logrus.SetLevel(logrus.DebugLevel)
	}
	helper := ecr.NewECRHelper(ecr.WithLogger(logger))
	return authn.NewKeychainFromHelper(newCachingHelper("amazon ecr", helper, ecrHelperCacheTTL, debugLog))
})

// ecrHelperCacheTTL bounds how long credentials from the ECR helper are reused
// in-process. An ECR authorization token is valid for 12 hours and the helper
// itself hands out a fresh one once half of that has passed, so a token served
// from the cache is never older than eleven hours, leaving an hour of margin.
const ecrHelperCacheTTL = 5 * time.Hour

// Environment variables naming the Bazel credential helper to use. The
// generic EnvCredentialHelper applies to every operation; the scoped variants
// override it for a single kind of operation and take precedence when set.
const (
	// EnvCredentialHelper is the credential helper used for every operation
	// unless a more specific variable is set.
	EnvCredentialHelper = "IMG_CREDENTIAL_HELPER"
	// EnvCredentialHelperOCIRegistry is the credential helper used for OCI
	// registry operations (push, pull, tag). Takes precedence over
	// EnvCredentialHelper for registry authentication.
	EnvCredentialHelperOCIRegistry = "IMG_CREDENTIAL_HELPER_OCI_REGISTRY"
	// EnvCredentialHelperRemoteCache is the credential helper used to
	// authenticate gRPC calls to the remote cache / remote execution API.
	// Takes precedence over EnvCredentialHelper for those calls.
	EnvCredentialHelperRemoteCache = "IMG_CREDENTIAL_HELPER_REMOTE_CACHE"

	// EnvRegistryAuthHost is the registry host for credentials supplied directly
	// through the environment.
	EnvRegistryAuthHost = "IMG_REGISTRY_AUTH_HOST"
	// EnvRegistryAuthUsername is the username for registry basic authentication.
	EnvRegistryAuthUsername = "IMG_REGISTRY_AUTH_USERNAME"
	// EnvRegistryAuthPassword is the password for registry basic authentication.
	EnvRegistryAuthPassword = "IMG_REGISTRY_AUTH_PASSWORD"
	// EnvRegistryAuthBearerToken is a ready-to-send registry bearer token.
	EnvRegistryAuthBearerToken = "IMG_REGISTRY_AUTH_BEARER_TOKEN"
)

// OCIRegistryCredentialHelper returns the credential helper configured for OCI
// registry operations, honoring EnvCredentialHelperOCIRegistry before falling
// back to the generic EnvCredentialHelper. Returns "" when neither is set.
func OCIRegistryCredentialHelper() string {
	return firstNonEmptyEnv(EnvCredentialHelperOCIRegistry, EnvCredentialHelper)
}

// RemoteCacheCredentialHelper returns the credential helper configured for
// remote cache / REAPI gRPC operations, honoring EnvCredentialHelperRemoteCache
// before falling back to the generic EnvCredentialHelper. Returns "" when
// neither is set.
func RemoteCacheCredentialHelper() string {
	return firstNonEmptyEnv(EnvCredentialHelperRemoteCache, EnvCredentialHelper)
}

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}

// WithAuthFromMultiKeychain returns a remote.Option that uses a MultiKeychain.
// If a credential helper is configured (IMG_CREDENTIAL_HELPER_OCI_REGISTRY, or
// the generic IMG_CREDENTIAL_HELPER), the Bazel credential helper is checked
// first. Host-scoped IMG_REGISTRY_AUTH_* credentials are checked next, followed
// by an inline Docker config (IMG_DOCKER_CONFIG_INLINE), before the default
// Docker, Google, and Amazon ECR keychains.
// If `IMG_AUTH_DEBUG` is set, each keychain resolution is logged to stderr.
func WithAuthFromMultiKeychain() remote.Option {
	return remote.WithAuthFromKeychain(keychainFromEnvironment())
}

// Keychain returns the [authn.Keychain] used to resolve registry credentials.
// It honors the same environment (IMG_CREDENTIAL_HELPER_OCI_REGISTRY,
// IMG_CREDENTIAL_HELPER, IMG_DOCKER_CONFIG_INLINE, IMG_REGISTRY_AUTH_*,
// IMG_AUTH_DEBUG) as WithAuthFromMultiKeychain and is intended for callers that
// need the raw keychain (for example to run the token exchange flow themselves).
func Keychain() authn.Keychain {
	return keychainFromEnvironment()
}

// authDebug reports whether IMG_AUTH_DEBUG asks for credential resolution to be
// logged to stderr.
func authDebug() bool {
	_, debug := os.LookupEnv("IMG_AUTH_DEBUG")
	return debug
}

func keychainFromEnvironment() authn.Keychain {
	debug := authDebug()

	var keychains []authn.Keychain

	if value := OCIRegistryCredentialHelper(); value != "" {
		opts := &credential.Options{CaptureStderr: true}
		if debug {
			opts.DebugLog = authDebugLog
		}
		bazel := credential.New(value, opts)
		keychain := credential.ContainerRegistryKeychain(bazel)
		keychains = append(keychains, namedKeychain("bazel credential helper", keychain, debug))
	}

	// Short-lived credentials scoped to one registry host. Tried before an
	// inline Docker config so a per-invocation override wins over a broader
	// stored config.
	keychains = append(keychains, namedKeychain("registry environment", environmentKeychain{}, debug))

	// An inline Docker config injected into the (potentially remote) action's
	// environment. Tried before the on-disk Docker config so an explicitly
	// injected credential wins over whatever config file happens to exist.
	if value := os.Getenv(EnvDockerConfigInline); value != "" {
		keychains = append(keychains, namedKeychain("inline docker config", newInlineDockerConfigKeychain(value), debug))
	}

	keychains = append(
		keychains,
		namedKeychain("docker config", authn.DefaultKeychain, debug),
		namedKeychain("google", google.Keychain, debug),
		namedKeychain("amazon ecr", amazonKeychain(), debug),
	)

	return authn.NewMultiKeychain(keychains...)
}

type environmentKeychain struct{}

func (environmentKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	host := os.Getenv(EnvRegistryAuthHost)
	username := os.Getenv(EnvRegistryAuthUsername)
	password := os.Getenv(EnvRegistryAuthPassword)
	bearerToken := os.Getenv(EnvRegistryAuthBearerToken)

	if host == "" && username == "" && password == "" && bearerToken == "" {
		return authn.Anonymous, nil
	}
	if host == "" {
		return nil, fmt.Errorf("%s is required when registry environment credentials are set", EnvRegistryAuthHost)
	}

	registry, err := name.NewRegistry(host, name.StrictValidation)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", EnvRegistryAuthHost, err)
	}

	if !strings.EqualFold(registry.RegistryStr(), target.RegistryStr()) {
		return authn.Anonymous, nil
	}

	if bearerToken != "" && (username != "" || password != "") {
		return nil, fmt.Errorf("%s is mutually exclusive with %s and %s", EnvRegistryAuthBearerToken, EnvRegistryAuthUsername, EnvRegistryAuthPassword)
	}
	if bearerToken == "" && (username == "" || password == "") {
		return nil, fmt.Errorf("set either %s or both %s and %s", EnvRegistryAuthBearerToken, EnvRegistryAuthUsername, EnvRegistryAuthPassword)
	}

	if bearerToken != "" {
		return authn.FromConfig(authn.AuthConfig{RegistryToken: bearerToken}), nil
	}
	return authn.FromConfig(authn.AuthConfig{
		Username: username,
		Password: password,
	}), nil
}

func namedKeychain(name string, kc authn.Keychain, debug bool) authn.Keychain {
	if !debug {
		return kc
	}
	return &debugKeychain{name: name, inner: kc}
}

type debugKeychain struct {
	name  string
	inner authn.Keychain
}

func (d *debugKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return d.ResolveContext(context.Background(), target)
}

func (d *debugKeychain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	auth, err := authn.Resolve(ctx, d.inner, target)
	if err != nil {
		fmt.Fprintf(authDebugLog, "keychain %q for %s: error: %v\n", d.name, target.RegistryStr(), err)
		return nil, err
	}
	if auth == authn.Anonymous {
		fmt.Fprintf(authDebugLog, "keychain %q for %s: no credentials, trying next\n", d.name, target.RegistryStr())
		return authn.Anonymous, nil
	}
	fmt.Fprintf(authDebugLog, "keychain %q for %s: found credentials\n", d.name, target.RegistryStr())
	return auth, nil
}

// authDebugLog is where IMG_AUTH_DEBUG output goes: stderr, with every line
// prefixed so it can be told apart from (and grepped out of) everything else,
// including the output of credential helpers and libraries that know nothing of
// the prefix.
var authDebugLog io.Writer = &prefixWriter{prefix: []byte("IMG_AUTH_DEBUG: "), out: os.Stderr}

// prefixWriter writes prefix at the start of every line written through it. It
// is shared by every source of debug output, so writes are serialized, but a
// line written in several pieces by one source can still be split by another.
type prefixWriter struct {
	prefix []byte
	out    io.Writer

	mu      sync.Mutex
	midLine bool
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var buf []byte
	for rest := p; len(rest) > 0; {
		if !w.midLine {
			buf = append(buf, w.prefix...)
			w.midLine = true
		}
		line, after, found := bytes.Cut(rest, []byte{'\n'})
		buf = append(buf, line...)
		if found {
			buf = append(buf, '\n')
			w.midLine = false
		}
		rest = after
	}
	if _, err := w.out.Write(buf); err != nil {
		return 0, err
	}
	return len(p), nil
}
