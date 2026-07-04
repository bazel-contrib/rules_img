package registry

import (
	"context"
	"fmt"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/credential"
	"github.com/malt3/go-containerregistry/pkg/authn"
	"github.com/malt3/go-containerregistry/pkg/v1/google"
	"github.com/malt3/go-containerregistry/pkg/v1/remote"
)

// WithAuthFromMultiKeychain returns a remote.Option that uses a MultiKeychain
// combining the default keychain and the Google keychain for authentication.
// If `IMG_CREDENTIAL_HELPER` is set in the environment, the Bazel credential helper will
// contribute to the keychain.
// If `IMG_AUTH_DEBUG` is set, each keychain resolution is logged to stderr.
// WARNING: keep in sync with the same function in pull_tool/pkg/auth/registry/registry.go.
func WithAuthFromMultiKeychain() remote.Option {
	_, debug := os.LookupEnv("IMG_AUTH_DEBUG")

	var keychains []authn.Keychain

	if value, ok := os.LookupEnv("IMG_CREDENTIAL_HELPER"); ok {
		bazel := credential.New(value, &credential.Options{CaptureStderr: true})
		keychain := credential.ContainerRegistryKeychain(bazel)
		keychains = append(keychains, namedKeychain("bazel credential helper", keychain, debug))
	}

	keychains = append(
		keychains,
		namedKeychain("docker config", authn.DefaultKeychain, debug),
		namedKeychain("google", google.Keychain, debug),
	)

	kc := authn.NewMultiKeychain(keychains...)

	return remote.WithAuthFromKeychain(kc)
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
