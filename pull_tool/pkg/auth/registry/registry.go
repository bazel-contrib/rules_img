package registry

import (
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// WithAuthFromMultiKeychain returns a remote.Option that uses a MultiKeychain
// combining the default keychain and the Google keychain for authentication.
// WARNING: keep in sync with the same function in img_tool/pkg/auth/registry/registry.go.
func WithAuthFromMultiKeychain() remote.Option {
	kc := authn.NewMultiKeychain(
		authn.DefaultKeychain,
		google.Keychain,
	)

	return remote.WithAuthFromKeychain(kc)
}
