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

// WithAuthFromMultiKeychain returns a remote.Option that uses a MultiKeychain.
// If `IMG_CREDENTIAL_HELPER` is set in the environment, the Bazel credential helper
// is checked before the default Docker, Google, and Amazon ECR keychains.
// If `IMG_AUTH_DEBUG` is set, each keychain resolution is logged to stderr.
func WithAuthFromMultiKeychain() remote.Option {
	return remote.WithAuthFromKeychain(keychainFromEnvironment())
}

func keychainFromEnvironment() authn.Keychain {
	_, debug := os.LookupEnv("IMG_AUTH_DEBUG")

	var keychains []authn.Keychain

	if value, ok := os.LookupEnv("IMG_CREDENTIAL_HELPER"); ok && value != "" {
		bazel := credential.New(value, &credential.Options{CaptureStderr: true})
		docker := credential.ContainerRegistryHelper(bazel)
		keychain := authn.NewKeychainFromHelper(docker)
		keychains = append(keychains, namedKeychain("bazel credential helper", keychain, debug))
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
