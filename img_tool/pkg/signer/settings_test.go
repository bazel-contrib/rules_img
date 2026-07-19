package signer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return p
}

func TestDiscoverAndResolve(t *testing.T) {
	cfgA := `{"schema_version":1,"mode":"command","tool":"toolA"}`
	cfgB := `{"schema_version":1,"mode":"command","tool":"toolB"}`
	pathA := writeTemp(t, cfgA)

	// Ingest cfgA via --sign_setting_file; make cfgB the default via a path.
	pathB := writeTemp(t, cfgB)
	store, err := Discover(nil, []string{pathA}, pathB)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !store.HasDefault() {
		t.Fatal("expected a default sign setting")
	}

	// Explicit setting that was ingested resolves to its own config.
	descA := &api.Descriptor{MediaType: api.SignSettingMediaType, Digest: digestOf([]byte(cfgA))}
	got, err := store.Resolve(descA, nil)
	if err != nil {
		t.Fatalf("Resolve(descA): %v", err)
	}
	if got.Tool != "toolA" {
		t.Errorf("resolved tool = %q, want toolA", got.Tool)
	}

	// No explicit setting falls back to the default (cfgB).
	got, err = store.Resolve(nil, nil)
	if err != nil {
		t.Fatalf("Resolve(nil): %v", err)
	}
	if got.Tool != "toolB" {
		t.Errorf("default tool = %q, want toolB", got.Tool)
	}

	// Unresolved explicit setting fills in from the default (fill-in semantics).
	descMissing := &api.Descriptor{Digest: "sha256:" + "00"}
	got, err = store.Resolve(descMissing, nil)
	if err != nil {
		t.Fatalf("Resolve(missing) with default: %v", err)
	}
	if got.Tool != "toolB" {
		t.Errorf("fill-in tool = %q, want toolB", got.Tool)
	}
}

func TestResolveNoDefaultErrors(t *testing.T) {
	store, err := Discover(nil, nil, "")
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := store.Resolve(nil, nil); err == nil {
		t.Error("expected error when no setting and no default")
	}
}

func TestDiscoverDefaultBySha256MustExist(t *testing.T) {
	if _, err := Discover(nil, nil, "sha256:deadbeef"); err == nil {
		t.Error("expected error for --default_sign_setting referencing an unknown digest")
	}
}

func TestParseConfigSchemaVersion(t *testing.T) {
	if _, err := parseConfig([]byte(`{"schema_version":2,"mode":"command","tool":"x"}`)); err == nil {
		t.Error("expected error for unsupported schema_version")
	}
}
