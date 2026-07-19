// Package signer implements the `img deploy` side of container image signing.
//
// It knows nothing about signature wire formats. All cryptographic work is
// delegated to external signer plugins over a subprocess RPC: `img` runs
// `<tool> sign-oci-artifact [flags]`, writes the JSON of the subject descriptor
// to the plugin's stdin, and reads an OCI image layout tar from its stdout. The
// manifests in that layout carry the OCI 1.1 `subject` field and are pushed to
// the subject's repository as referrers by the caller.
//
// The single api.OCIArtifactSigner implementation here is Subprocess.
package signer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/bazelbuild/rules_go/go/runfiles"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// SignSettingsRunfilesDir is the fixed runfiles root-symlink area that holds the
// sign_setting config files shipped by the push/load/multi_deploy rules. Each
// entry has a unique basename (sha256 of the producing file's path); the deploy
// tool discovers them by content digest, so basenames are irrelevant beyond
// uniqueness.
const SignSettingsRunfilesDir = "++rules_img_private++/sign_settings"

// SignSettingConfig is the on-disk schema of a sign_setting config file,
// produced deterministically by the `signing_config` Bazel rule. It describes
// how to invoke a signer plugin and carries no key material.
type SignSettingConfig struct {
	SchemaVersion int               `json:"schema_version"`
	Mode          string            `json:"mode"` // "rlocation" or "command"
	Tool          string            `json:"tool"` // runfiles rlocation path or host command
	Args          []string          `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
}

// SettingStore maps a sign_setting content digest ("sha256:<hex>") to the raw
// config bytes, and records a default digest resolved from
// --default_sign_setting.
type SettingStore struct {
	byDigest      map[string][]byte
	defaultDigest string
}

// Discover builds a SettingStore from the runfiles sign_settings area plus any
// files passed via --sign_setting_file, and resolves the --default_sign_setting
// value (a "sha256:" digest that must already be present, or a path to ingest).
// A nil runfiles handle (or a missing sign_settings dir) is not an error.
func Discover(rf *runfiles.Runfiles, extraFiles []string, defaultSetting string) (*SettingStore, error) {
	s := &SettingStore{byDigest: map[string][]byte{}}

	if rf != nil {
		walkErr := fs.WalkDir(rf, SignSettingsRunfilesDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// A missing sign_settings dir simply means nothing was configured.
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			f, err := rf.Open(path)
			if err != nil {
				return fmt.Errorf("opening sign_setting %s: %w", path, err)
			}
			data, err := io.ReadAll(f)
			_ = f.Close()
			if err != nil {
				return fmt.Errorf("reading sign_setting %s: %w", path, err)
			}
			s.add(data)
			return nil
		})
		if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
			return nil, fmt.Errorf("discovering sign settings in runfiles: %w", walkErr)
		}
	}

	for _, p := range extraFiles {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading --sign_setting_file %s: %w", p, err)
		}
		s.add(data)
	}

	if defaultSetting != "" {
		if strings.HasPrefix(defaultSetting, "sha256:") {
			if _, ok := s.byDigest[defaultSetting]; !ok {
				return nil, fmt.Errorf("--default_sign_setting %s not found among discovered sign settings", defaultSetting)
			}
			s.defaultDigest = defaultSetting
		} else {
			data, err := os.ReadFile(defaultSetting)
			if err != nil {
				return nil, fmt.Errorf("reading --default_sign_setting %s: %w", defaultSetting, err)
			}
			s.defaultDigest = s.add(data)
		}
	}

	return s, nil
}

// HasDefault reports whether a runtime default sign setting is available.
func (s *SettingStore) HasDefault() bool { return s.defaultDigest != "" }

// add records config bytes by their content digest and returns that digest.
func (s *SettingStore) add(data []byte) string {
	sum := sha256.Sum256(data)
	d := "sha256:" + hex.EncodeToString(sum[:])
	s.byDigest[d] = data
	return d
}

// Resolve returns the config bytes to use for a push operation. It prefers the
// operation's explicit setting; if that is absent or not discovered, it falls
// back to the runtime default (--default_sign_setting), then to the manifest
// default (DeploySettings.DefaultSignSetting).
func (s *SettingStore) Resolve(setting, manifestDefault *api.Descriptor) (SignSettingConfig, error) {
	var cfg SignSettingConfig
	if setting != nil && setting.Digest != "" {
		if data, ok := s.byDigest[setting.Digest]; ok {
			return parseConfig(data)
		}
	}
	def := s.defaultDigest
	if def == "" && manifestDefault != nil {
		def = manifestDefault.Digest
	}
	if def != "" {
		if data, ok := s.byDigest[def]; ok {
			return parseConfig(data)
		}
	}
	if setting != nil && setting.Digest != "" {
		return cfg, fmt.Errorf("sign_setting %s not found in runfiles or via --sign_setting_file, and no usable default", setting.Digest)
	}
	return cfg, fmt.Errorf("no sign_setting configured and no default available")
}

func parseConfig(data []byte) (SignSettingConfig, error) {
	var cfg SignSettingConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parsing sign_setting config: %w", err)
	}
	if cfg.SchemaVersion != 1 {
		return cfg, fmt.Errorf("unsupported sign_setting schema_version %d (want 1)", cfg.SchemaVersion)
	}
	return cfg, nil
}
