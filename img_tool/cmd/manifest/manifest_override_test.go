package manifest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	specv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func strPtr(s string) *string { return &s }

func slicePtr(items ...string) *[]string {
	v := append([]string(nil), items...)
	return &v
}

// resetManifestFlags clears the package-level flag variables so each test case
// starts from a clean slate (the legacy flags must not leak between cases).
func resetManifestFlags() {
	operatingSystem = "linux"
	architecture = "amd64"
	variant = ""
	user = ""
	env = stringMap{}
	envFile = ""
	entrypoint = stringList{}
	cmd = stringList{}
	workingDir = ""
	labels = stringMap{}
	stopSignal = ""
}

// TestConfigOverridesNULRoundTrip verifies the transport property the design
// relies on: the NUL-carrying sentinel is JSON-escaped on the wire (no literal
// NUL byte, so it can live in a file), and decodes back into the exact
// inheritFromBase constant. Go's encoding/json escapes NUL identically to
// Starlark's json.encode, so the bytes here match what the rule writes.
func TestConfigOverridesNULRoundTrip(t *testing.T) {
	in := &configOverrides{
		User:       strPtr(inheritFromBase),
		WorkingDir: strPtr(inheritFromBase),
		StopSignal: strPtr(inheritFromBase),
		Entrypoint: slicePtr(inheritFromBase),
		Cmd:        slicePtr(inheritFromBase, "--flag"),
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The wire form must not contain a literal NUL byte (it could not otherwise be
	// written to / read from a file, nor pass through a command line).
	if bytes.IndexByte(data, 0) != -1 {
		t.Fatalf("marshaled overrides contain a literal NUL byte: %q", data)
	}

	// Round-trip through the actual file reader.
	dir := t.TempDir()
	path := filepath.Join(dir, "overrides.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing overrides file: %v", err)
	}
	ov, err := readConfigOverrides(path)
	if err != nil {
		t.Fatalf("readConfigOverrides: %v", err)
	}
	if ov.User == nil || *ov.User != inheritFromBase {
		t.Errorf("user = %q, want sentinel %q", derefString(ov.User), inheritFromBase)
	}
	if ov.Entrypoint == nil || len(*ov.Entrypoint) != 1 || (*ov.Entrypoint)[0] != inheritFromBase {
		t.Errorf("entrypoint = %v, want [sentinel]", ov.Entrypoint)
	}
	if ov.Cmd == nil || len(*ov.Cmd) != 2 || (*ov.Cmd)[0] != inheritFromBase || (*ov.Cmd)[1] != "--flag" {
		t.Errorf("cmd = %v, want [sentinel, --flag]", ov.Cmd)
	}

	// Also verify the *literal* bytes Starlark's json.encode produces decode
	// correctly. Unlike Go's json.Marshal, Starlark does not HTML-escape < and >,
	// and it writes the NUL as a six-character backslash-u-0000 escape. We build
	// that escape from a raw backslash (0x5c) so this test source has no NUL byte.
	backslash := string(rune(0x5c))
	esc := backslash + "u0000" // JSON escape for a NUL byte
	starlarkWire := `{"user":"<inherit from base>` + esc + `","cmd":["<inherit from base>` + esc + `","--flag"]}`
	if bytes.IndexByte([]byte(starlarkWire), 0) != -1 {
		t.Fatalf("test fixture unexpectedly contains a literal NUL: %q", starlarkWire)
	}
	wirePath := filepath.Join(dir, "starlark_wire.json")
	if err := os.WriteFile(wirePath, []byte(starlarkWire), 0o644); err != nil {
		t.Fatalf("writing starlark wire file: %v", err)
	}
	wireOv, err := readConfigOverrides(wirePath)
	if err != nil {
		t.Fatalf("readConfigOverrides (starlark wire): %v", err)
	}
	if wireOv.User == nil || *wireOv.User != inheritFromBase {
		t.Errorf("starlark-wire user = %q, want sentinel %q", derefString(wireOv.User), inheritFromBase)
	}
	if wireOv.Cmd == nil || len(*wireOv.Cmd) != 2 || (*wireOv.Cmd)[0] != inheritFromBase || (*wireOv.Cmd)[1] != "--flag" {
		t.Errorf("starlark-wire cmd = %v, want [sentinel, --flag]", wireOv.Cmd)
	}
}

func TestOverlayNewConfigValuesOverrides(t *testing.T) {
	base := specv1.ImageConfig{
		User:       "baseuser",
		WorkingDir: "/base",
		StopSignal: "SIGTERM",
		Entrypoint: []string{"/base-entry"},
		Cmd:        []string{"base-arg"},
	}

	tests := []struct {
		name           string
		ov             *configOverrides
		wantUser       string
		wantWorkingDir string
		wantStopSignal string
		wantEntrypoint []string
		wantCmd        []string
	}{
		{
			name: "all inherit (sentinel defaults)",
			ov: &configOverrides{
				User:       strPtr(inheritFromBase),
				WorkingDir: strPtr(inheritFromBase),
				StopSignal: strPtr(inheritFromBase),
				Entrypoint: slicePtr(inheritFromBase),
				Cmd:        slicePtr(inheritFromBase),
			},
			wantUser:       "baseuser",
			wantWorkingDir: "/base",
			wantStopSignal: "SIGTERM",
			wantEntrypoint: []string{"/base-entry"},
			wantCmd:        []string{"base-arg"},
		},
		{
			name: "all unset (explicit empty)",
			ov: &configOverrides{
				User:       strPtr(""),
				WorkingDir: strPtr(""),
				StopSignal: strPtr(""),
				Entrypoint: slicePtr(),
				Cmd:        slicePtr(),
			},
			wantUser:       "",
			wantWorkingDir: "",
			wantStopSignal: "",
			wantEntrypoint: nil,
			wantCmd:        nil,
		},
		{
			name: "explicit values override",
			ov: &configOverrides{
				User:       strPtr("nobody"),
				WorkingDir: strPtr("/app"),
				StopSignal: strPtr("SIGKILL"),
				Entrypoint: slicePtr("/app/bin"),
				Cmd:        slicePtr("--serve"),
			},
			wantUser:       "nobody",
			wantWorkingDir: "/app",
			wantStopSignal: "SIGKILL",
			wantEntrypoint: []string{"/app/bin"},
			// entrypoint was set -> inherited cmd cleared, then cmd override applies
			wantCmd: []string{"--serve"},
		},
		{
			name: "entrypoint appends to base, cmd inherits (cleared by entrypoint set)",
			ov: &configOverrides{
				Entrypoint: slicePtr(inheritFromBase, "--verbose"),
				Cmd:        slicePtr(inheritFromBase),
			},
			wantUser:       "baseuser", // no override for user -> legacy flag path (empty) -> inherit
			wantWorkingDir: "/base",
			wantStopSignal: "SIGTERM",
			wantEntrypoint: []string{"/base-entry", "--verbose"},
			// entrypoint explicitly set clears inherited cmd; cmd sentinel is a no-op
			wantCmd: nil,
		},
		{
			name: "cmd appends to base, entrypoint inherits",
			ov: &configOverrides{
				Entrypoint: slicePtr(inheritFromBase),
				Cmd:        slicePtr(inheritFromBase, "extra"),
			},
			wantUser:       "baseuser",
			wantWorkingDir: "/base",
			wantStopSignal: "SIGTERM",
			wantEntrypoint: []string{"/base-entry"}, // pure inherit -> untouched
			wantCmd:        []string{"base-arg", "extra"},
		},
		{
			name: "entrypoint set, cmd explicitly unset",
			ov: &configOverrides{
				Entrypoint: slicePtr("/new-entry"),
				Cmd:        slicePtr(),
			},
			wantUser:       "baseuser",
			wantWorkingDir: "/base",
			wantStopSignal: "SIGTERM",
			wantEntrypoint: []string{"/new-entry"},
			wantCmd:        nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetManifestFlags()
			cfg := specv1.Image{Config: base}
			// deep-copy the slices so the shared base fixture is not mutated
			cfg.Config.Entrypoint = append([]string(nil), base.Entrypoint...)
			cfg.Config.Cmd = append([]string(nil), base.Cmd...)

			if err := overlayNewConfigValues(&cfg, nil, nil, tt.ov); err != nil {
				t.Fatalf("overlayNewConfigValues: %v", err)
			}
			if cfg.Config.User != tt.wantUser {
				t.Errorf("User = %q, want %q", cfg.Config.User, tt.wantUser)
			}
			if cfg.Config.WorkingDir != tt.wantWorkingDir {
				t.Errorf("WorkingDir = %q, want %q", cfg.Config.WorkingDir, tt.wantWorkingDir)
			}
			if cfg.Config.StopSignal != tt.wantStopSignal {
				t.Errorf("StopSignal = %q, want %q", cfg.Config.StopSignal, tt.wantStopSignal)
			}
			if !reflect.DeepEqual(nilIfEmpty(cfg.Config.Entrypoint), tt.wantEntrypoint) {
				t.Errorf("Entrypoint = %v, want %v", cfg.Config.Entrypoint, tt.wantEntrypoint)
			}
			if !reflect.DeepEqual(nilIfEmpty(cfg.Config.Cmd), tt.wantCmd) {
				t.Errorf("Cmd = %v, want %v", cfg.Config.Cmd, tt.wantCmd)
			}
		})
	}
}

// TestOverlayNewConfigValuesLegacyFlags ensures the historical flag-based path
// (no --config-overrides file) keeps its two-way "empty inherits" behavior.
func TestOverlayNewConfigValuesLegacyFlags(t *testing.T) {
	base := specv1.ImageConfig{
		User:       "baseuser",
		Entrypoint: []string{"/base-entry"},
		Cmd:        []string{"base-arg"},
	}
	resetManifestFlags()
	// Simulate legacy flags: only entrypoint set, everything else empty.
	entrypoint = stringList{"/new-entry"}

	cfg := specv1.Image{Config: base}
	cfg.Config.Entrypoint = append([]string(nil), base.Entrypoint...)
	cfg.Config.Cmd = append([]string(nil), base.Cmd...)

	if err := overlayNewConfigValues(&cfg, nil, nil, nil); err != nil {
		t.Fatalf("overlayNewConfigValues: %v", err)
	}
	if cfg.Config.User != "baseuser" {
		t.Errorf("User = %q, want inherited baseuser", cfg.Config.User)
	}
	if !reflect.DeepEqual(cfg.Config.Entrypoint, []string{"/new-entry"}) {
		t.Errorf("Entrypoint = %v, want [/new-entry]", cfg.Config.Entrypoint)
	}
	if cfg.Config.Cmd != nil {
		t.Errorf("Cmd = %v, want nil (cleared by entrypoint)", cfg.Config.Cmd)
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

func derefString(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
