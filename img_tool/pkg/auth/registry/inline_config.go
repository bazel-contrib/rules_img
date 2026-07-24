package registry

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/configfile"
	"github.com/docker/cli/cli/config/types"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// EnvDockerConfigInline holds a Docker configuration document inline: its value
// is the JSON contents of a `config.json` (the same format as
// `~/.docker/config.json`), not a path to one.
//
// It is resolved exactly like the Docker config file keychain, but entirely in
// memory, so it works in environments where no config file exists on disk.
//
// General use is discouraged. Passing it on a command line or through a Bazel
// setting would leak the credentials into the build event stream, action
// metadata, logs, and the remote cache. It is meant only for mechanisms that
// inject the variable directly into a (potentially remote) action's process
// environment — for example BuildBuddy secrets under remote execution. See
// docs/authenticating-build-actions.md.
const EnvDockerConfigInline = "IMG_DOCKER_CONFIG_INLINE"

// inlineDockerConfigKeychain resolves registry credentials from a Docker config
// JSON document held in memory rather than read from disk. It mirrors the
// resolution semantics of [authn.DefaultKeychain] so an inline config behaves
// like the equivalent file would.
type inlineDockerConfigKeychain struct {
	// mu guards resolution: configfile.GetAuthConfig lazily initializes internal
	// maps, so concurrent resolves (e.g. a multi-arch index push) must serialize,
	// matching authn.DefaultKeychain.
	mu  sync.Mutex
	cf  *configfile.ConfigFile
	err error
}

// newInlineDockerConfigKeychain parses raw (the JSON contents of a Docker
// config.json) once. A parse error is retained and surfaced from every Resolve
// so a malformed value fails loudly instead of being silently ignored.
func newInlineDockerConfigKeychain(raw string) *inlineDockerConfigKeychain {
	cf, err := config.LoadFromReader(strings.NewReader(raw))
	if err != nil {
		return &inlineDockerConfigKeychain{err: fmt.Errorf("parsing %s: %w", EnvDockerConfigInline, err)}
	}
	return &inlineDockerConfigKeychain{cf: cf}
}

func (k *inlineDockerConfigKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return k.ResolveContext(context.Background(), target)
}

func (k *inlineDockerConfigKeychain) ResolveContext(_ context.Context, target authn.Resource) (authn.Authenticator, error) {
	if k.err != nil {
		return nil, k.err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	// Mirror authn.DefaultKeychain: try the full reference and then the bare
	// registry, mapping Docker Hub to its historical config key.
	var cfg, empty types.AuthConfig
	for _, key := range []string{target.String(), target.RegistryStr()} {
		if key == name.DefaultRegistry {
			key = authn.DefaultAuthKey
		}
		var err error
		cfg, err = k.cf.GetAuthConfig(key)
		if err != nil {
			return nil, err
		}
		// GetAuthConfig sets ServerAddress; clear it so the "is empty" test below
		// works (see go-containerregistry#1510).
		cfg.ServerAddress = ""
		if cfg != empty {
			break
		}
	}
	if cfg == empty {
		return authn.Anonymous, nil
	}

	return authn.FromConfig(authn.AuthConfig{
		Username:      cfg.Username,
		Password:      cfg.Password,
		Auth:          cfg.Auth,
		IdentityToken: cfg.IdentityToken,
		RegistryToken: cfg.RegistryToken,
	}), nil
}

var _ authn.Keychain = (*inlineDockerConfigKeychain)(nil)
