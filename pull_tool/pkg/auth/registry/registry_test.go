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
