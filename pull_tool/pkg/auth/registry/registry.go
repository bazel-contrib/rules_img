package registry

import (
	"io"
	"sync"

	ecr "github.com/awslabs/amazon-ecr-credential-helper/ecr-login"
	"github.com/chrismellard/docker-credential-acr-env/pkg/credhelper"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/authn/github"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

var (
	amazonKeychain = sync.OnceValue(func() authn.Keychain {
		return authn.NewKeychainFromHelper(ecr.NewECRHelper(ecr.WithLogger(io.Discard)))
	})
	azureKeychain = sync.OnceValue(func() authn.Keychain {
		return authn.NewKeychainFromHelper(credhelper.NewACRCredentialsHelper())
	})
)

// WithAuthFromMultiKeychain returns a remote.Option that uses a MultiKeychain
// combining the default keychain and the Google keychain for authentication.
// WARNING: keep in sync with the same function in img_tool/pkg/auth/registry/registry.go.
func WithAuthFromMultiKeychain() remote.Option {
	kc := authn.NewMultiKeychain(
		authn.DefaultKeychain,
		google.Keychain,
		github.Keychain,
		amazonKeychain(),
		azureKeychain(),
	)

	return remote.WithAuthFromKeychain(kc)
}
