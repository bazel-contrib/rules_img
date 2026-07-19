package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	specv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// resetManifestFlags clears the package-level flag variables so each test case
// starts from a clean slate (flags must not leak between cases).
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
	additionalImageLabelsFile = ""
	stopSignal = ""
}

func TestOverlayNewConfigValues(t *testing.T) {
	base := specv1.ImageConfig{
		User:       "baseuser",
		WorkingDir: "/base",
		StopSignal: "SIGTERM",
		Entrypoint: []string{"/base-entry"},
		Cmd:        []string{"base-arg"},
	}

	tests := []struct {
		name           string
		user           string
		workingDir     string
		stopSignal     string
		entrypoint     stringList
		cmd            stringList
		wantUser       string
		wantWorkingDir string
		wantStopSignal string
		wantEntrypoint []string
		wantCmd        []string
	}{
		{
			name:           "all inherit (sentinel defaults)",
			user:           inheritFromBase,
			workingDir:     inheritFromBase,
			stopSignal:     inheritFromBase,
			entrypoint:     stringList{inheritFromBase},
			cmd:            stringList{inheritFromBase},
			wantUser:       "baseuser",
			wantWorkingDir: "/base",
			wantStopSignal: "SIGTERM",
			wantEntrypoint: []string{"/base-entry"},
			wantCmd:        []string{"base-arg"},
		},
		{
			name:           "all unset (explicit empty)",
			user:           "",
			workingDir:     "",
			stopSignal:     "",
			entrypoint:     stringList{},
			cmd:            stringList{},
			wantUser:       "",
			wantWorkingDir: "",
			wantStopSignal: "",
			wantEntrypoint: nil,
			wantCmd:        nil,
		},
		{
			name:           "explicit values override",
			user:           "nobody",
			workingDir:     "/app",
			stopSignal:     "SIGKILL",
			entrypoint:     stringList{"/app/bin"},
			cmd:            stringList{"--serve"},
			wantUser:       "nobody",
			wantWorkingDir: "/app",
			wantStopSignal: "SIGKILL",
			wantEntrypoint: []string{"/app/bin"},
			// entrypoint was set -> inherited cmd cleared, then cmd override applies
			wantCmd: []string{"--serve"},
		},
		{
			name:           "entrypoint appends to base, cmd inherits (cleared by entrypoint set)",
			user:           inheritFromBase,
			workingDir:     inheritFromBase,
			stopSignal:     inheritFromBase,
			entrypoint:     stringList{inheritFromBase, "--verbose"},
			cmd:            stringList{inheritFromBase},
			wantUser:       "baseuser",
			wantWorkingDir: "/base",
			wantStopSignal: "SIGTERM",
			wantEntrypoint: []string{"/base-entry", "--verbose"},
			// entrypoint explicitly set clears inherited cmd; cmd sentinel is a no-op
			wantCmd: nil,
		},
		{
			name:           "cmd appends to base, entrypoint inherits",
			user:           inheritFromBase,
			workingDir:     inheritFromBase,
			stopSignal:     inheritFromBase,
			entrypoint:     stringList{inheritFromBase},
			cmd:            stringList{inheritFromBase, "extra"},
			wantUser:       "baseuser",
			wantWorkingDir: "/base",
			wantStopSignal: "SIGTERM",
			wantEntrypoint: []string{"/base-entry"}, // pure inherit -> untouched
			wantCmd:        []string{"base-arg", "extra"},
		},
		{
			name:           "entrypoint set, cmd explicitly unset",
			user:           inheritFromBase,
			workingDir:     inheritFromBase,
			stopSignal:     inheritFromBase,
			entrypoint:     stringList{"/new-entry"},
			cmd:            stringList{},
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
			user = tt.user
			workingDir = tt.workingDir
			stopSignal = tt.stopSignal
			entrypoint = tt.entrypoint
			cmd = tt.cmd

			cfg := specv1.Image{Config: base}
			// deep-copy the slices so the shared base fixture is not mutated
			cfg.Config.Entrypoint = append([]string(nil), base.Entrypoint...)
			cfg.Config.Cmd = append([]string(nil), base.Cmd...)

			if err := overlayNewConfigValues(&cfg, nil, nil); err != nil {
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

// TestOverlayNewConfigValuesNoBase verifies that inheriting or expanding against
// an absent base leaves the field empty rather than erroring.
func TestOverlayNewConfigValuesNoBase(t *testing.T) {
	resetManifestFlags()
	user = inheritFromBase
	workingDir = inheritFromBase
	stopSignal = inheritFromBase
	entrypoint = stringList{inheritFromBase}
	cmd = stringList{inheritFromBase, "extra"} // sentinel expands to nothing

	cfg := specv1.Image{} // no base config

	if err := overlayNewConfigValues(&cfg, nil, nil); err != nil {
		t.Fatalf("overlayNewConfigValues: %v", err)
	}
	if cfg.Config.User != "" {
		t.Errorf("User = %q, want empty", cfg.Config.User)
	}
	if len(cfg.Config.Entrypoint) != 0 {
		t.Errorf("Entrypoint = %v, want empty", cfg.Config.Entrypoint)
	}
	if !reflect.DeepEqual(cfg.Config.Cmd, []string{"extra"}) {
		t.Errorf("Cmd = %v, want [extra]", cfg.Config.Cmd)
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// TestOverlayNewConfigValuesAdditionalLabels verifies that labels from the
// --additional-image-labels-file are merged into the config, that per-target
// --label values take precedence over file entries for matching keys, and that
// an empty file contributes nothing.
func TestOverlayNewConfigValuesAdditionalLabels(t *testing.T) {
	dir := t.TempDir()

	writeFile := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		return path
	}

	t.Run("file only", func(t *testing.T) {
		resetManifestFlags()
		additionalImageLabelsFile = writeFile("labels.txt", "team=platform\ntier=backend\n")

		cfg := specv1.Image{}
		if err := overlayNewConfigValues(&cfg, nil, nil); err != nil {
			t.Fatalf("overlayNewConfigValues: %v", err)
		}
		want := map[string]string{"team": "platform", "tier": "backend"}
		if !reflect.DeepEqual(cfg.Config.Labels, want) {
			t.Errorf("Labels = %v, want %v", cfg.Config.Labels, want)
		}
	})

	t.Run("per-target label overrides file", func(t *testing.T) {
		resetManifestFlags()
		additionalImageLabelsFile = writeFile("labels2.txt", "team=platform\nowner=fromfile\n")
		labels = stringMap{"owner": "fromflag", "extra": "flagonly"}

		cfg := specv1.Image{}
		if err := overlayNewConfigValues(&cfg, nil, nil); err != nil {
			t.Fatalf("overlayNewConfigValues: %v", err)
		}
		want := map[string]string{
			"team":  "platform", // from file
			"owner": "fromflag", // --label wins over file
			"extra": "flagonly", // from --label
		}
		if !reflect.DeepEqual(cfg.Config.Labels, want) {
			t.Errorf("Labels = %v, want %v", cfg.Config.Labels, want)
		}
	})

	t.Run("JSON file", func(t *testing.T) {
		resetManifestFlags()
		additionalImageLabelsFile = writeFile("labels.json", `{"team":"platform","tier":"backend"}`)

		cfg := specv1.Image{}
		if err := overlayNewConfigValues(&cfg, nil, nil); err != nil {
			t.Fatalf("overlayNewConfigValues: %v", err)
		}
		want := map[string]string{"team": "platform", "tier": "backend"}
		if !reflect.DeepEqual(cfg.Config.Labels, want) {
			t.Errorf("Labels = %v, want %v", cfg.Config.Labels, want)
		}
	})

	t.Run("empty file is a no-op", func(t *testing.T) {
		resetManifestFlags()
		additionalImageLabelsFile = writeFile("empty.txt", "")

		cfg := specv1.Image{}
		if err := overlayNewConfigValues(&cfg, nil, nil); err != nil {
			t.Fatalf("overlayNewConfigValues: %v", err)
		}
		if len(cfg.Config.Labels) != 0 {
			t.Errorf("Labels = %v, want empty", cfg.Config.Labels)
		}
	})
}
