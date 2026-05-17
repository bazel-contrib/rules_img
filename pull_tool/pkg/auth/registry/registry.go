package registry

import (
	"os"

	"github.com/bazel-contrib/rules_img/pull_tool/pkg/auth/credential"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// WithAuthFromMultiKeychain returns a remote.Option that uses a MultiKeychain.
// If `IMG_CREDENTIAL_HELPER` is set in the environment, the Bazel credential helper
// is checked before the default Docker and Google keychains.
// WARNING: keep in sync with the same function in img_tool/pkg/auth/registry/registry.go.
func WithAuthFromMultiKeychain() remote.Option {
	return remote.WithAuthFromKeychain(keychainFromEnvironment())
}

func keychainFromEnvironment() authn.Keychain {
	keychains := []authn.Keychain{}

	if value, ok := os.LookupEnv("IMG_CREDENTIAL_HELPER"); ok && value != "" {
		bazel := credential.New(value)
		docker := credential.ContainerRegistryHelper(bazel)
		keychain := authn.NewKeychainFromHelper(docker)
		keychains = append(keychains, keychain)
	}

	keychains = append(keychains,
		authn.DefaultKeychain,
		google.Keychain,
	)

	return authn.NewMultiKeychain(keychains...)
}
