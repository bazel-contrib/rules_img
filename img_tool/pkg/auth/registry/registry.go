package registry

import (
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
// WARNING: keep in sync with the same function in pull_tool/pkg/auth/registry/registry.go.
func WithAuthFromMultiKeychain() remote.Option {
	keychains := []authn.Keychain{
		authn.DefaultKeychain,
		google.Keychain,
	}

	if value, ok := os.LookupEnv("IMG_CREDENTIAL_HELPER"); ok {
		bazel := credential.New(value)
		docker := credential.ContainerRegistryHelper(bazel)
		keychain := authn.NewKeychainFromHelper(docker)
		keychains = append(keychains, keychain)
	}

	kc := authn.NewMultiKeychain(keychains...)

	return remote.WithAuthFromKeychain(kc)
}
