package credential

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
)

// When RULES_IMG_TEST_CREDENTIAL_HELPER_CALLS is set, the test binary acts as a
// credential helper: it records the invocation by appending a line to that file
// and answers with a header and, if RULES_IMG_TEST_CREDENTIAL_HELPER_EXPIRES is
// set, that expiry. It writes RULES_IMG_TEST_CREDENTIAL_HELPER_STDERR, if set,
// to stderr.
func TestMain(m *testing.M) {
	if calls := os.Getenv("RULES_IMG_TEST_CREDENTIAL_HELPER_CALLS"); calls != "" && len(os.Args) > 1 && os.Args[1] == "get" {
		f, err := os.OpenFile(calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Fprintln(f, "get")
		f.Close()
		fmt.Fprint(os.Stderr, os.Getenv("RULES_IMG_TEST_CREDENTIAL_HELPER_STDERR"))
		resp, _ := json.Marshal(externalResponse{
			Expires: os.Getenv("RULES_IMG_TEST_CREDENTIAL_HELPER_EXPIRES"),
			Headers: map[string][]string{"Authorization": {"Bearer token"}},
		})
		os.Stdout.Write(resp)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// newSelfHelper returns a helper running this test binary (see TestMain),
// answering with expires, and a function counting its invocations so far.
func newSelfHelper(t *testing.T, expires string) (Helper, func() int) {
	t.Helper()
	return newSelfHelperWithOptions(t, expires, &Options{CaptureStderr: true})
}

func newSelfHelperWithOptions(t *testing.T, expires string, opts *Options) (Helper, func() int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	t.Setenv("RULES_IMG_TEST_CREDENTIAL_HELPER_CALLS", calls)
	t.Setenv("RULES_IMG_TEST_CREDENTIAL_HELPER_EXPIRES", expires)
	count := func() int {
		data, err := os.ReadFile(calls)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		} else if err != nil {
			t.Fatal(err)
		}
		return strings.Count(string(data), "\n")
	}
	return New(self, opts), count
}

func TestExternalHelper_DebugLog(t *testing.T) {
	t.Setenv("RULES_IMG_TEST_CREDENTIAL_HELPER_STDERR", "helper says hi\n")
	var debugLog bytes.Buffer
	helper, _ := newSelfHelperWithOptions(t, "", &Options{CaptureStderr: true, DebugLog: &debugLog})

	for range 2 {
		if _, _, err := helper.Get(t.Context(), "registry.example"); err != nil {
			t.Fatal(err)
		}
	}

	log := debugLog.String()
	// Only the first lookup runs the helper; the second is a cache hit.
	if got := strings.Count(log, "running for registry.example"); got != 1 {
		t.Errorf("debug log mentions %d runs, want 1:\n%s", got, log)
	}
	if got := strings.Count(log, "helper says hi"); got != 1 {
		t.Errorf("debug log carries the helper's stderr %d times, want 1:\n%s", got, log)
	}
	if !strings.Contains(log, "got credentials without an expiry") {
		t.Errorf("debug log does not report the result:\n%s", log)
	}
}

func TestExternalHelper_CachesForDefaultExpiryWithoutExpires(t *testing.T) {
	helper, calls := newSelfHelper(t, "")

	before := time.Now()
	_, expiresAt, err := helper.Get(t.Context(), "registry.example")
	after := time.Now()
	if err != nil {
		t.Fatal(err)
	}
	// A response without an expiry is reused for an hour.
	if expiresAt.Before(before.Add(60*time.Minute)) || expiresAt.After(after.Add(60*time.Minute)) {
		t.Errorf("expiresAt = %v, want 60m after the lookup (between %v and %v)", expiresAt, before.Add(60*time.Minute), after.Add(60*time.Minute))
	}

	headers, cachedExpiresAt, err := helper.Get(t.Context(), "registry.example")
	if err != nil {
		t.Fatal(err)
	}
	if got := headers["Authorization"]; len(got) != 1 || got[0] != "Bearer token" {
		t.Errorf("cached headers = %v, want the helper's", headers)
	}
	if !cachedExpiresAt.Equal(expiresAt) {
		t.Errorf("cached expiresAt = %v, want %v", cachedExpiresAt, expiresAt)
	}
	if got := calls(); got != 1 {
		t.Errorf("helper ran %d times for two lookups of one URI, want 1", got)
	}

	if _, _, err := helper.Get(t.Context(), "other.example"); err != nil {
		t.Fatal(err)
	}
	if got := calls(); got != 2 {
		t.Errorf("helper ran %d times after a second URI, want 2", got)
	}
}

func TestExternalHelper_HonorsExpires(t *testing.T) {
	// Longer than the default, so the helper's expiry visibly wins.
	want := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	helper, calls := newSelfHelper(t, want.Format(time.RFC3339))

	for range 2 {
		_, expiresAt, err := helper.Get(t.Context(), "registry.example")
		if err != nil {
			t.Fatal(err)
		}
		if !expiresAt.Equal(want) {
			t.Errorf("expiresAt = %v, want the helper's %v", expiresAt, want)
		}
	}
	if got := calls(); got != 1 {
		t.Errorf("helper ran %d times for two lookups of one URI, want 1", got)
	}
}

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

func TestNew_WorkspacePlaceholderWithoutEnv(t *testing.T) {
	orig, hadOrig := os.LookupEnv("BUILD_WORKSPACE_DIRECTORY")
	defer func() {
		if hadOrig {
			os.Setenv("BUILD_WORKSPACE_DIRECTORY", orig)
		} else {
			os.Unsetenv("BUILD_WORKSPACE_DIRECTORY")
		}
	}()
	os.Unsetenv("BUILD_WORKSPACE_DIRECTORY")

	helper := New("%workspace%/bin/helper", nil)
	extHelper, ok := helper.(*externalCredentialHelper)
	if !ok {
		t.Fatalf("expected *externalCredentialHelper, got %T", helper)
	}
	// Without BUILD_WORKSPACE_DIRECTORY, the placeholder is left unresolved
	// (and a warning is printed to stderr).
	expected := "%workspace%/bin/helper"
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
