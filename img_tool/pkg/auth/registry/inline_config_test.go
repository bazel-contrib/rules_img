package registry

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

// clearCredentialHelperEnv unsets the credential-helper variables so the tests
// exercise the inline-config keychain without an ambient helper shadowing it.
func clearCredentialHelperEnv(t *testing.T) {
	t.Helper()
	t.Setenv("IMG_CREDENTIAL_HELPER", "")
	t.Setenv("IMG_CREDENTIAL_HELPER_OCI_REGISTRY", "")
}

func resolveAuth(t *testing.T, ref string) (username, password string) {
	t.Helper()
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatalf("failed to parse registry reference: %v", err)
	}
	authenticator, err := keychainFromEnvironment().Resolve(parsed.Context().Registry)
	if err != nil {
		t.Fatalf("failed to resolve credentials: %v", err)
	}
	authConfig, err := authenticator.Authorization()
	if err != nil {
		t.Fatalf("failed to resolve auth config: %v", err)
	}
	return authConfig.Username, authConfig.Password
}

func TestKeychainFromEnvironmentUsesInlineDockerConfig(t *testing.T) {
	clearCredentialHelperEnv(t)
	// Ensure no on-disk Docker config can satisfy the lookup instead.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv(EnvDockerConfigInline, `{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`)

	if user, pass := resolveAuth(t, "registry.example.com/project/image:tag"); user != "user" || pass != "pass" {
		t.Fatalf("expected user/pass from inline docker config, got %q/%q", user, pass)
	}
}

func TestInlineDockerConfigTakesPrecedenceOverDockerConfigFile(t *testing.T) {
	clearCredentialHelperEnv(t)

	dockerConfigDir := t.TempDir()
	fileConfig := []byte(`{"auths":{"registry.example.com":{"auth":"ZmlsZS11c2VyOmZpbGUtcGFzcw=="}}}`)
	if err := os.WriteFile(filepath.Join(dockerConfigDir, "config.json"), fileConfig, 0o600); err != nil {
		t.Fatalf("failed to write Docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfigDir)
	t.Setenv(EnvDockerConfigInline, `{"auths":{"registry.example.com":{"auth":"aW5saW5lLXVzZXI6aW5saW5lLXBhc3M="}}}`)

	if user, pass := resolveAuth(t, "registry.example.com/project/image:tag"); user != "inline-user" || pass != "inline-pass" {
		t.Fatalf("expected inline config to win over Docker config file, got %q/%q", user, pass)
	}
}

func TestInlineDockerConfigResolvesUsernamePassword(t *testing.T) {
	clearCredentialHelperEnv(t)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	// GHA and `docker login` may store username/password directly rather than a
	// combined base64 `auth`; both must resolve.
	t.Setenv(EnvDockerConfigInline, `{"auths":{"registry.example.com":{"username":"u2","password":"p2"}}}`)

	if user, pass := resolveAuth(t, "registry.example.com/project/image:tag"); user != "u2" || pass != "p2" {
		t.Fatalf("expected user/pass from inline docker config, got %q/%q", user, pass)
	}
}

func TestInlineDockerConfigUsesDockerHubKey(t *testing.T) {
	clearCredentialHelperEnv(t)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	// Docker Hub is stored under the historical key; a bare Docker Hub reference
	// must still resolve against it.
	t.Setenv(EnvDockerConfigInline, `{"auths":{"https://index.docker.io/v1/":{"auth":"aHViLXVzZXI6aHViLXBhc3M="}}}`)

	if user, pass := resolveAuth(t, "ubuntu:latest"); user != "hub-user" || pass != "hub-pass" {
		t.Fatalf("expected user/pass from Docker Hub inline entry, got %q/%q", user, pass)
	}
}

func TestInlineDockerConfigFallsThroughForOtherRegistry(t *testing.T) {
	clearCredentialHelperEnv(t)

	dockerConfigDir := t.TempDir()
	fileConfig := []byte(`{"auths":{"registry.example.com":{"auth":"ZmlsZS11c2VyOmZpbGUtcGFzcw=="}}}`)
	if err := os.WriteFile(filepath.Join(dockerConfigDir, "config.json"), fileConfig, 0o600); err != nil {
		t.Fatalf("failed to write Docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfigDir)
	// The inline config only has credentials for another registry, so the lookup
	// falls through to the Docker config file.
	t.Setenv(EnvDockerConfigInline, `{"auths":{"other.example.com":{"auth":"aW5saW5lLXVzZXI6aW5saW5lLXBhc3M="}}}`)

	if user, pass := resolveAuth(t, "registry.example.com/project/image:tag"); user != "file-user" || pass != "file-pass" {
		t.Fatalf("expected fall-through to Docker config file, got %q/%q", user, pass)
	}
}

func TestInlineDockerConfigEmptyIsIgnored(t *testing.T) {
	clearCredentialHelperEnv(t)

	dockerConfigDir := t.TempDir()
	fileConfig := []byte(`{"auths":{"registry.example.com":{"auth":"ZmlsZS11c2VyOmZpbGUtcGFzcw=="}}}`)
	if err := os.WriteFile(filepath.Join(dockerConfigDir, "config.json"), fileConfig, 0o600); err != nil {
		t.Fatalf("failed to write Docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfigDir)
	t.Setenv(EnvDockerConfigInline, "")

	if user, pass := resolveAuth(t, "registry.example.com/project/image:tag"); user != "file-user" || pass != "file-pass" {
		t.Fatalf("expected Docker config file to be used when inline is empty, got %q/%q", user, pass)
	}
}

func TestInlineDockerConfigInvalidJSONErrors(t *testing.T) {
	clearCredentialHelperEnv(t)
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv(EnvDockerConfigInline, "this is not json")

	ref, err := name.ParseReference("registry.example.com/project/image:tag")
	if err != nil {
		t.Fatalf("failed to parse registry reference: %v", err)
	}
	if _, err := keychainFromEnvironment().Resolve(ref.Context().Registry); err == nil {
		t.Fatal("expected a malformed inline docker config to fail loudly, got nil error")
	}
}
