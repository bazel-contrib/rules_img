package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

func TestMain(m *testing.M) {
	if os.Getenv("RULES_IMG_TEST_CREDENTIAL_HELPER") == "1" && len(os.Args) > 1 && os.Args[1] == "get" {
		fmt.Fprint(os.Stdout, `{"headers":{"Authorization":["Basic dXNlcjpwYXNz"]}}`)
		os.Exit(0)
	}

	os.Exit(m.Run())
}

func TestKeychainFromEnvironmentPrefersConfiguredCredentialHelper(t *testing.T) {
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatalf("failed to locate test executable: %v", err)
	}

	t.Setenv("RULES_IMG_TEST_CREDENTIAL_HELPER", "1")
	t.Setenv("IMG_CREDENTIAL_HELPER", helperPath)

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
