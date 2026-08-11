package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the committed description of a pre-release channel: which module is
// published, where the registry is served from, and which container repository
// holds the tool binaries the published lockfiles point at.
//
// It is read by this tool and by the workflow that drives it, so that a fork has
// one file to edit rather than a workflow to rewrite.
type Config struct {
	// Module is the name of the Bazel module being published.
	Module string `json:"module"`
	// RegistryURL is the base URL the registry is served from, i.e. what a
	// consumer passes to --registry.
	RegistryURL string `json:"registry_url"`
	// PagesBranch is the branch the registry lives on. Only the workflow acts on
	// it; it is recorded here so both halves agree.
	PagesBranch string `json:"pages_branch"`
	// AssetBaseURL is where the source archives are served from. Defaults to
	// <RegistryURL>/assets, which is where this tool writes them.
	AssetBaseURL string `json:"asset_base_url,omitempty"`
	// MetadataTemplate is a BCR-style metadata.json used to seed a module's
	// metadata the first time it is published. A relative path is resolved
	// against the directory holding this config.
	MetadataTemplate string `json:"metadata_template,omitempty"`
	// Artifact locates the ORAS artifact whose layers are the tool binaries.
	Artifact Artifact `json:"artifact"`
}

// Artifact is the location of the ORAS artifact holding the tool binaries and
// the lockfile that names them.
type Artifact struct {
	Registry      string `json:"registry"`
	Repository    string `json:"repository"`
	LockfileTitle string `json:"lockfile_title,omitempty"`
}

const defaultLockfileTitle = "prebuilt_lockfile.json"

// LoadConfig reads and validates the channel configuration.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}
	if cfg.Artifact.LockfileTitle == "" {
		cfg.Artifact.LockfileTitle = defaultLockfileTitle
	}
	if cfg.MetadataTemplate != "" && !filepath.IsAbs(cfg.MetadataTemplate) {
		cfg.MetadataTemplate = filepath.Join(filepath.Dir(path), cfg.MetadataTemplate)
	}
	cfg.RegistryURL = strings.TrimSuffix(cfg.RegistryURL, "/")
	if cfg.AssetBaseURL == "" {
		cfg.AssetBaseURL = cfg.RegistryURL + "/assets"
	}
	cfg.AssetBaseURL = strings.TrimSuffix(cfg.AssetBaseURL, "/")

	var missing []string
	if cfg.Module == "" {
		missing = append(missing, "module")
	}
	if cfg.RegistryURL == "" {
		missing = append(missing, "registry_url")
	}
	if cfg.Artifact.Registry == "" {
		missing = append(missing, "artifact.registry")
	}
	if cfg.Artifact.Repository == "" {
		missing = append(missing, "artifact.repository")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("config %s is missing %s", path, strings.Join(missing, ", "))
	}
	return &cfg, nil
}
