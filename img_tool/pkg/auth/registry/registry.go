package registry

import (
	"context"
	"fmt"
	"io"
	"os"

	ecr "github.com/awslabs/amazon-ecr-credential-helper/ecr-login"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/google/go-containerregistry/pkg/authn"
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
// first. An inline Docker config (IMG_DOCKER_CONFIG_INLINE) is checked next,
// before the default Docker, Google, and Amazon ECR keychains.
// If `IMG_AUTH_DEBUG` is set, each keychain resolution is logged to stderr.
func WithAuthFromMultiKeychain() remote.Option {
	return remote.WithAuthFromKeychain(keychainFromEnvironment())
}

// Keychain returns the [authn.Keychain] used to resolve registry credentials.
// It honors the same environment (IMG_CREDENTIAL_HELPER_OCI_REGISTRY,
// IMG_CREDENTIAL_HELPER, IMG_DOCKER_CONFIG_INLINE, IMG_AUTH_DEBUG) as
// WithAuthFromMultiKeychain and is intended for callers that need the raw
// keychain (for example to run the token exchange flow themselves).
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
