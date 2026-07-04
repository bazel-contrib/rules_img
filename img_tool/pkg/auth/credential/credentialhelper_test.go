package credential

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/malt3/go-containerregistry/pkg/authn"
)

func TestNew_ReplacesWorkspacePlaceholder(t *testing.T) {
	// Set up environment variable
	orig := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	defer os.Setenv("BUILD_WORKSPACE_DIRECTORY", orig)
	os.Setenv("BUILD_WORKSPACE_DIRECTORY", "/tmp/workspace")

	helper := New("%workspace%/bin/helper", nil)
	extHelper, ok := helper.(*externalCredentialHelper)
	if !ok {
		t.Fatalf("expected *externalCredentialHelper, got %T", helper)
	}
	expected := "/tmp/workspace/bin/helper"
	if extHelper.helperBinary != expected {
		t.Errorf("expected helperBinary to be %q, got %q", expected, extHelper.helperBinary)
	}
}

func TestNew_WithoutWorkspacePlaceholder(t *testing.T) {
	orig := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	defer os.Setenv("BUILD_WORKSPACE_DIRECTORY", orig)
	os.Setenv("BUILD_WORKSPACE_DIRECTORY", "/tmp/workspace")

	helper := New("/usr/local/bin/helper", nil)
	extHelper, ok := helper.(*externalCredentialHelper)
	if !ok {
		t.Fatalf("expected *externalCredentialHelper, got %T", helper)
	}
	expected := "/usr/local/bin/helper"
	if extHelper.helperBinary != expected {
		t.Errorf("expected helperBinary to be %q, got %q", expected, extHelper.helperBinary)
	}
}

type TestHelper struct {
	Headers map[string][]string
}

func (t *TestHelper) Get(_ context.Context, _ string) (headers map[string][]string, expiresAt time.Time, err error) {
	return t.Headers, time.Time{}, nil
}

type testResource struct {
	registry string
}

func (r testResource) String() string {
	return r.registry
}

func (r testResource) RegistryStr() string {
	return r.registry
}

func TestContainerRegistryHelper_WithNilHeaders(t *testing.T) {
	helper := TestHelper{}
	crh := ContainerRegistryHelper(&helper)
	_, _, err := crh.Get("")
	if err == nil {
		t.Fatalf("expected err to be not nil")
	} else if msg := err.Error(); msg != "no HTTP headers found" {
		t.Fatalf(`expected error to be "no HTTP headers found", got %s`, msg)
	}
}

func TestContainerRegistryHelper_WithNoHeaders(t *testing.T) {
	helper := TestHelper{
		Headers: map[string][]string{},
	}
	crh := ContainerRegistryHelper(&helper)
	_, _, err := crh.Get("")
	if err == nil {
		t.Fatalf("expected err to be not nil")
	} else if msg := err.Error(); msg != "no `Authorization` header" {
		t.Fatalf("expected error to be \"no `Authorization` header\", got %s", msg)
	}
}

func TestContainerRegistryHelper_WithNoScheme(t *testing.T) {
	helper := TestHelper{
		Headers: map[string][]string{
			"Authorization": []string{"no-space-here"},
		},
	}
	crh := ContainerRegistryHelper(&helper)
	_, _, err := crh.Get("")
	if err == nil {
		t.Fatalf("expected err to be not nil")
	} else if msg := err.Error(); msg != "no authorization scheme: no-space-here" {
		t.Fatalf("expected error to be \"no authorization scheme: no-space-here\", got %s", msg)
	}
}

func TestContainerRegistryHelper_WithBasicAuthIncorrectData(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("no-semi-colon"))
	helper := TestHelper{
		Headers: map[string][]string{
			"Authorization": []string{"Basic " + encoded},
		},
	}
	crh := ContainerRegistryHelper(&helper)
	_, _, err := crh.Get("")
	if err == nil {
		t.Fatalf("expected err to be not nil")
	} else if msg := err.Error(); msg != "no semi-colon in basic auth: no-semi-colon" {
		t.Fatalf("expected error to be \"no semi-colon in basic auth: no-semi-colon\", got %s", msg)
	}
}

func TestContainerRegistryHelper_WithBasicAuthIncorrectEncoding(t *testing.T) {
	helper := TestHelper{
		Headers: map[string][]string{
			"Authorization": []string{"Basic !"},
		},
	}
	crh := ContainerRegistryHelper(&helper)
	_, _, err := crh.Get("")
	if err == nil {
		t.Fatalf("expected err to be not nil")
	} else if msg := err.Error(); !strings.HasPrefix(msg, "decode authorisation header: Basic !") {
		t.Fatalf("expected error to be \"decode authorisation header: Basic !\", got %s", msg)
	}
}

func TestContainerRegistryHelper_WithBasicAuth(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("test:pass"))
	helper := TestHelper{
		Headers: map[string][]string{
			"Authorization": []string{"Basic " + encoded},
		},
	}
	crh := ContainerRegistryHelper(&helper)
	username, password, err := crh.Get("")
	if err != nil {
		t.Fatalf("expected err to be nil, got %v", err)
	} else if username != "test" {
		t.Fatalf(`expected username to be "test", got %s`, username)
	} else if password != "pass" {
		t.Fatalf(`expected username to be "pass", got %s`, password)
	}
}

func TestContainerRegistryHelper_WithBearerAuth(t *testing.T) {
	helper := TestHelper{
		Headers: map[string][]string{
			"Authorization": []string{"Bearer <token>"},
		},
	}
	crh := ContainerRegistryHelper(&helper)
	username, password, err := crh.Get("")
	if err != nil {
		t.Fatalf("expected err to be nil, got %v", err)
	} else if username != "<token>" {
		t.Fatalf(`expected username to be "<token>", got %s`, username)
	} else if password != "<token>" {
		t.Fatalf(`expected username to be "<token>", got %s`, password)
	}
}

func TestContainerRegistryHelper_WithUnknownScheme(t *testing.T) {
	helper := TestHelper{
		Headers: map[string][]string{
			"Authorization": []string{"Unknown ..."},
		},
	}
	crh := ContainerRegistryHelper(&helper)
	_, _, err := crh.Get("")
	if err == nil {
		t.Fatalf("expected err to be not nil")
	} else if msg := err.Error(); msg != "unknown authorization scheme: Unknown ..." {
		t.Fatalf("expected error to be \"unknown authorization scheme: Unknown ...\", got %s", msg)
	}
}

func TestContainerRegistryHelper_WithEmptyAuthHeader(t *testing.T) {
	helper := TestHelper{
		Headers: map[string][]string{
			"Authorization": []string{},
		},
	}
	crh := ContainerRegistryHelper(&helper)
	_, _, err := crh.Get("")
	if err == nil {
		t.Fatalf("expected err to be not nil")
	} else if msg := err.Error(); msg != "no `Authorization` headers" {
		t.Fatalf("expected error to be \"no `Authorization` headers\", got %s", msg)
	}
}

func TestContainerRegistryKeychain_WithBearerAuth(t *testing.T) {
	helper := TestHelper{
		Headers: map[string][]string{
			"Authorization": []string{"Bearer access-token"},
		},
	}
	keychain := ContainerRegistryKeychain(&helper)

	auth, err := keychain.Resolve(testResource{registry: "registry.example.com"})
	if err != nil {
		t.Fatalf("expected err to be nil, got %v", err)
	}
	cfg, err := authn.Authorization(context.Background(), auth)
	if err != nil {
		t.Fatalf("expected err to be nil, got %v", err)
	}
	if cfg.RegistryToken != "access-token" {
		t.Fatalf("expected registry token to be %q, got %q", "access-token", cfg.RegistryToken)
	}
	if cfg.IdentityToken != "" {
		t.Fatalf("expected identity token to be empty, got %q", cfg.IdentityToken)
	}
}

func TestContainerRegistryKeychain_WithBasicAuth(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("test:pass"))
	helper := TestHelper{
		Headers: map[string][]string{
			"Authorization": []string{"Basic " + encoded},
		},
	}
	keychain := ContainerRegistryKeychain(&helper)

	auth, err := keychain.Resolve(testResource{registry: "registry.example.com"})
	if err != nil {
		t.Fatalf("expected err to be nil, got %v", err)
	}
	cfg, err := authn.Authorization(context.Background(), auth)
	if err != nil {
		t.Fatalf("expected err to be nil, got %v", err)
	}
	if cfg.Username != "test" {
		t.Fatalf(`expected username to be "test", got %s`, cfg.Username)
	}
	if cfg.Password != "pass" {
		t.Fatalf(`expected password to be "pass", got %s`, cfg.Password)
	}
	if cfg.RegistryToken != "" {
		t.Fatalf("expected registry token to be empty, got %q", cfg.RegistryToken)
	}
}
