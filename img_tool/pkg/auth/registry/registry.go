package registry

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	ecr "github.com/awslabs/amazon-ecr-credential-helper/ecr-login"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// amazonKeychain authenticates to Amazon ECR registries using the
// amazon-ecr-credential-helper, resolving credentials from the ambient AWS
// configuration (environment, shared config files, or instance/role metadata).
var amazonKeychain authn.Keychain = authn.NewKeychainFromHelper(ecr.NewECRHelper(ecr.WithLogger(io.Discard)))

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

func keychainFromEnvironment() authn.Keychain {
	_, debug := os.LookupEnv("IMG_AUTH_DEBUG")

	var keychains []authn.Keychain

	if value := OCIRegistryCredentialHelper(); value != "" {
		bazel := credential.New(value, &credential.Options{CaptureStderr: true})
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
		namedKeychain("amazon ecr", amazonKeychain, debug),
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
		fmt.Fprintf(os.Stderr, "IMG_AUTH_DEBUG: keychain %q for %s: error: %v\n", d.name, target.RegistryStr(), err)
		return nil, err
	}
	if auth == authn.Anonymous {
		fmt.Fprintf(os.Stderr, "IMG_AUTH_DEBUG: keychain %q for %s: no credentials, trying next\n", d.name, target.RegistryStr())
		return authn.Anonymous, nil
	}
	fmt.Fprintf(os.Stderr, "IMG_AUTH_DEBUG: keychain %q for %s: found credentials\n", d.name, target.RegistryStr())
	return auth, nil
}
