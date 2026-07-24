package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

func TestMain(m *testing.M) {
	if os.Getenv("RULES_IMG_TEST_CREDENTIAL_HELPER") == "1" && len(os.Args) > 1 && os.Args[1] == "get" {
		fmt.Fprint(os.Stdout, `{"headers":{"Authorization":["Basic dXNlcjpwYXNz"]}}`)
		os.Exit(0)
	}

	os.Exit(m.Run())
}

func clearRegistryAuthEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"IMG_CREDENTIAL_HELPER",
		"IMG_CREDENTIAL_HELPER_OCI_REGISTRY",
		EnvDockerConfigInline,
		EnvRegistryAuthHost,
		EnvRegistryAuthUsername,
		EnvRegistryAuthPassword,
		EnvRegistryAuthBearerToken,
	} {
		t.Setenv(name, "")
	}
}

func TestKeychainFromEnvironmentPrefersConfiguredCredentialHelper(t *testing.T) {
	clearRegistryAuthEnvironment(t)

	helperPath, err := os.Executable()
	if err != nil {
		t.Fatalf("failed to locate test executable: %v", err)
	}

	t.Setenv("RULES_IMG_TEST_CREDENTIAL_HELPER", "1")
	t.Setenv("IMG_CREDENTIAL_HELPER", helperPath)
	t.Setenv(EnvRegistryAuthHost, "registry.example.com")
	t.Setenv(EnvRegistryAuthUsername, "environment-user")
	t.Setenv(EnvRegistryAuthPassword, "environment-pass")
	t.Setenv(EnvDockerConfigInline, `{"auths":{"registry.example.com":{"auth":"aW5saW5lLXVzZXI6aW5saW5lLXBhc3M="}}}`)

	dockerConfigDir := t.TempDir()
	dockerConfig := []byte(`{"credsStore":"rules-img-missing-helper"}`)
	if err := os.WriteFile(filepath.Join(dockerConfigDir, "config.json"), dockerConfig, 0o600); err != nil {
		t.Fatalf("failed to write Docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfigDir)

	ref, err := name.ParseReference("registry.example.com/project/image:tag")
	if err != nil {
		t.Fatalf("failed to parse registry reference: %v", err)
	}

	authenticator, err := keychainFromEnvironment().Resolve(ref.Context().Registry)
	if err != nil {
		t.Fatalf("credential helper should be used before Docker config: %v", err)
	}

	authConfig, err := authenticator.Authorization()
	if err != nil {
		t.Fatalf("failed to resolve auth config: %v", err)
	}
	if authConfig.Username != "user" || authConfig.Password != "pass" {
		t.Fatalf("expected user/pass from credential helper, got %q/%q", authConfig.Username, authConfig.Password)
	}
}

func TestKeychainFromEnvironmentIgnoresEmptyCredentialHelper(t *testing.T) {
	clearRegistryAuthEnvironment(t)

	t.Setenv("IMG_CREDENTIAL_HELPER", "")

	dockerConfigDir := t.TempDir()
	dockerConfig := []byte(`{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`)
	if err := os.WriteFile(filepath.Join(dockerConfigDir, "config.json"), dockerConfig, 0o600); err != nil {
		t.Fatalf("failed to write Docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfigDir)

	ref, err := name.ParseReference("registry.example.com/project/image:tag")
	if err != nil {
		t.Fatalf("failed to parse registry reference: %v", err)
	}

	authenticator, err := keychainFromEnvironment().Resolve(ref.Context().Registry)
	if err != nil {
		t.Fatalf("empty credential helper should fall through to Docker config: %v", err)
	}

	authConfig, err := authenticator.Authorization()
	if err != nil {
		t.Fatalf("failed to resolve auth config: %v", err)
	}
	if authConfig.Username != "user" || authConfig.Password != "pass" {
		t.Fatalf("expected user/pass from Docker config, got %q/%q", authConfig.Username, authConfig.Password)
	}
}

func TestKeychainFromEnvironmentPrefersOCIRegistryHelper(t *testing.T) {
	clearRegistryAuthEnvironment(t)

	helperPath, err := os.Executable()
	if err != nil {
		t.Fatalf("failed to locate test executable: %v", err)
	}

	t.Setenv("RULES_IMG_TEST_CREDENTIAL_HELPER", "1")
	// The registry-scoped helper is set; the generic one is empty. Registry auth
	// must use the scoped helper regardless.
	t.Setenv("IMG_CREDENTIAL_HELPER", "")
	t.Setenv("IMG_CREDENTIAL_HELPER_OCI_REGISTRY", helperPath)

	dockerConfigDir := t.TempDir()
	dockerConfig := []byte(`{"credsStore":"rules-img-missing-helper"}`)
	if err := os.WriteFile(filepath.Join(dockerConfigDir, "config.json"), dockerConfig, 0o600); err != nil {
		t.Fatalf("failed to write Docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfigDir)

	ref, err := name.ParseReference("registry.example.com/project/image:tag")
	if err != nil {
		t.Fatalf("failed to parse registry reference: %v", err)
	}

	authenticator, err := keychainFromEnvironment().Resolve(ref.Context().Registry)
	if err != nil {
		t.Fatalf("registry-scoped credential helper should be used before Docker config: %v", err)
	}

	authConfig, err := authenticator.Authorization()
	if err != nil {
		t.Fatalf("failed to resolve auth config: %v", err)
	}
	if authConfig.Username != "user" || authConfig.Password != "pass" {
		t.Fatalf("expected user/pass from credential helper, got %q/%q", authConfig.Username, authConfig.Password)
	}
}

func TestKeychainFromEnvironmentPrefersRegistryEnvironment(t *testing.T) {
	clearRegistryAuthEnvironment(t)
	t.Setenv("IMG_CREDENTIAL_HELPER", "")
	t.Setenv(EnvRegistryAuthHost, "registry.example.com")
	t.Setenv(EnvRegistryAuthUsername, "environment-user")
	t.Setenv(EnvRegistryAuthPassword, "environment-pass")
	t.Setenv(EnvDockerConfigInline, `{"auths":{"registry.example.com":{"auth":"aW5saW5lLXVzZXI6aW5saW5lLXBhc3M="}}}`)

	dockerConfigDir := t.TempDir()
	dockerConfig := []byte(`{"auths":{"registry.example.com":{"auth":"ZG9ja2VyOnBhc3M="}}}`)
	if err := os.WriteFile(filepath.Join(dockerConfigDir, "config.json"), dockerConfig, 0o600); err != nil {
		t.Fatalf("failed to write Docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfigDir)

	ref, err := name.ParseReference("registry.example.com/project/image:tag")
	if err != nil {
		t.Fatalf("failed to parse registry reference: %v", err)
	}

	authenticator, err := keychainFromEnvironment().Resolve(ref.Context().Registry)
	if err != nil {
		t.Fatalf("registry environment should be used before injected and on-disk Docker configs: %v", err)
	}

	authConfig, err := authenticator.Authorization()
	if err != nil {
		t.Fatalf("failed to resolve auth config: %v", err)
	}
	if authConfig.Username != "environment-user" || authConfig.Password != "environment-pass" {
		t.Fatalf("expected user/pass from registry environment, got %q/%q", authConfig.Username, authConfig.Password)
	}
}

func TestEnvironmentKeychainBearerToken(t *testing.T) {
	clearRegistryAuthEnvironment(t)
	t.Setenv(EnvRegistryAuthHost, "registry.example.com")
	t.Setenv(EnvRegistryAuthBearerToken, "registry-token")

	ref, err := name.ParseReference("registry.example.com/project/image:tag")
	if err != nil {
		t.Fatalf("failed to parse registry reference: %v", err)
	}

	authenticator, err := (environmentKeychain{}).Resolve(ref.Context().Registry)
	if err != nil {
		t.Fatalf("failed to resolve registry environment: %v", err)
	}
	authConfig, err := authenticator.Authorization()
	if err != nil {
		t.Fatalf("failed to resolve auth config: %v", err)
	}
	if authConfig.RegistryToken != "registry-token" {
		t.Fatalf("expected bearer token from registry environment, got %q", authConfig.RegistryToken)
	}
}

func TestEnvironmentKeychainNormalizesDockerHub(t *testing.T) {
	clearRegistryAuthEnvironment(t)
	t.Setenv(EnvRegistryAuthHost, "docker.io")
	t.Setenv(EnvRegistryAuthUsername, "user")
	t.Setenv(EnvRegistryAuthPassword, "pass")

	ref, err := name.ParseReference("ubuntu:latest")
	if err != nil {
		t.Fatalf("failed to parse registry reference: %v", err)
	}

	authenticator, err := (environmentKeychain{}).Resolve(ref.Context().Registry)
	if err != nil {
		t.Fatalf("failed to resolve registry environment: %v", err)
	}
	if authenticator == authn.Anonymous {
		t.Fatal("expected docker.io credentials to match index.docker.io")
	}
}

func TestEnvironmentKeychainIgnoresOtherRegistries(t *testing.T) {
	clearRegistryAuthEnvironment(t)
	t.Setenv(EnvRegistryAuthHost, "registry.example.com")
	t.Setenv(EnvRegistryAuthUsername, "incomplete-credentials")

	ref, err := name.ParseReference("other.example.com/project/image:tag")
	if err != nil {
		t.Fatalf("failed to parse registry reference: %v", err)
	}

	authenticator, err := (environmentKeychain{}).Resolve(ref.Context().Registry)
	if err != nil {
		t.Fatalf("failed to resolve registry environment: %v", err)
	}
	if authenticator != authn.Anonymous {
		t.Fatal("expected credentials to be ignored for another registry")
	}
}

func TestEnvironmentKeychainRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		username    string
		password    string
		bearerToken string
	}{
		{
			name:     "missing host",
			username: "user",
			password: "pass",
		},
		{
			name:     "missing password",
			host:     "registry.example.com",
			username: "user",
		},
		{
			name:        "mixed authentication",
			host:        "registry.example.com",
			username:    "user",
			password:    "pass",
			bearerToken: "registry-token",
		},
		{
			name:     "invalid host",
			host:     "https://registry.example.com",
			username: "user",
			password: "pass",
		},
	}

	ref, err := name.ParseReference("registry.example.com/project/image:tag")
	if err != nil {
		t.Fatalf("failed to parse registry reference: %v", err)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearRegistryAuthEnvironment(t)
			t.Setenv(EnvRegistryAuthHost, test.host)
			t.Setenv(EnvRegistryAuthUsername, test.username)
			t.Setenv(EnvRegistryAuthPassword, test.password)
			t.Setenv(EnvRegistryAuthBearerToken, test.bearerToken)

			if _, err := (environmentKeychain{}).Resolve(ref.Context().Registry); err == nil {
				t.Fatal("expected invalid registry environment to fail")
			}
		})
	}
}

func TestOCIRegistryCredentialHelperPrecedence(t *testing.T) {
	t.Setenv("IMG_CREDENTIAL_HELPER", "generic")
	t.Setenv("IMG_CREDENTIAL_HELPER_REMOTE_CACHE", "cache")
	t.Setenv("IMG_CREDENTIAL_HELPER_OCI_REGISTRY", "registry")
	if got := OCIRegistryCredentialHelper(); got != "registry" {
		t.Fatalf("expected registry-scoped helper to take precedence, got %q", got)
	}
}

func TestOCIRegistryCredentialHelperFallsBackToGeneric(t *testing.T) {
	t.Setenv("IMG_CREDENTIAL_HELPER", "generic")
	t.Setenv("IMG_CREDENTIAL_HELPER_OCI_REGISTRY", "")
	if got := OCIRegistryCredentialHelper(); got != "generic" {
		t.Fatalf("expected fallback to generic helper, got %q", got)
	}
}

func TestRemoteCacheCredentialHelperPrecedence(t *testing.T) {
	t.Setenv("IMG_CREDENTIAL_HELPER", "generic")
	t.Setenv("IMG_CREDENTIAL_HELPER_OCI_REGISTRY", "registry")
	t.Setenv("IMG_CREDENTIAL_HELPER_REMOTE_CACHE", "cache")
	if got := RemoteCacheCredentialHelper(); got != "cache" {
		t.Fatalf("expected remote-cache-scoped helper to take precedence, got %q", got)
	}
}

func TestRemoteCacheCredentialHelperFallsBackToGeneric(t *testing.T) {
	t.Setenv("IMG_CREDENTIAL_HELPER", "generic")
	t.Setenv("IMG_CREDENTIAL_HELPER_REMOTE_CACHE", "")
	if got := RemoteCacheCredentialHelper(); got != "generic" {
		t.Fatalf("expected fallback to generic helper, got %q", got)
	}
}
