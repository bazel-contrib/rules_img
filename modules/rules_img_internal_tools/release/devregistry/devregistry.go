// Publishes one pre-release version of a Bazel module into a registry served as
// a static site, so that a commit can be depended on before it is released.
//
// The module source is the archive a release build produced; the version is a
// pre-release of the next version up, stamped with the date and commit it was
// built from. Archives are stored by content, so a commit that leaves the
// module's sources alone adds nothing to the registry but a few hundred bytes of
// metadata: the prebuilt lockfile that *is* specific to the commit is layered
// onto the archive per version instead of being packaged into it.
//
// The version's MODULE.bazel is patched to declare the pre-release version, and
// the module's list of versions is merged rather than replaced -- a registry is
// append-only, because a version somebody already resolved has to keep resolving.
//
// The tool only writes files. Turning the result into a commit on the branch the
// site is served from is left to whatever drives it.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	configPath       string
	publishVersion   string
	archivePath      string
	lockfilePath     string
	outputDir        string
	assetBaseURL     string
	metadataTemplate string
)

func main() {
	flag.StringVar(&configPath, "config", "", "Path to the pre-release channel configuration (required).")
	flag.StringVar(&publishVersion, "version", "", "Version to publish, e.g. 0.4.0-20260811-fa8b7de (required).")
	flag.StringVar(&archivePath, "archive", "", "Path to the module's source archive, a .tar.gz (required).")
	flag.StringVar(&lockfilePath, "lockfile", "", "Path to the prebuilt lockfile to publish as this version's overlay (required).")
	flag.StringVar(&outputDir, "output", "", "Directory holding the registry, i.e. a checkout of the branch it is served from (required).")
	flag.StringVar(&assetBaseURL, "asset-base-url", "", "Overrides the configured base URL of the archives. Useful to resolve a registry locally, e.g. file:///tmp/registry/assets.")
	flag.StringVar(&metadataTemplate, "metadata-template", "", "Overrides the configured metadata.json template used when a module is published for the first time.")
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "devregistry: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	for name, value := range map[string]string{
		"--config":   configPath,
		"--version":  publishVersion,
		"--archive":  archivePath,
		"--lockfile": lockfilePath,
		"--output":   outputDir,
	} {
		if value == "" {
			return fmt.Errorf("missing required %s", name)
		}
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	if assetBaseURL != "" {
		cfg.AssetBaseURL = assetBaseURL
	}
	if metadataTemplate != "" {
		cfg.MetadataTemplate = metadataTemplate
	}

	writer := &registryWriter{root: outputDir}
	return publish(cfg, writer, publishVersion, archivePath, lockfilePath)
}

func publish(cfg *Config, writer *registryWriter, publishVersion, archivePath, lockfilePath string) error {
	// The archive is the source of truth for what is being published, and it is
	// deliberately not specific to the commit: the lockfile that is, is layered
	// on top of it per version rather than packaged into it, so that commits which
	// leave the module's sources alone reuse the archive already stored.
	moduleContent, err := readFromArchive(archivePath, moduleBazel)
	if err != nil {
		return err
	}
	declared, err := declaredVersion(moduleContent)
	if err != nil {
		return err
	}
	if err := validatePrereleaseVersion(publishVersion, declared); err != nil {
		return err
	}
	if err := checkArchiveShipsNoLockfile(cfg, archivePath); err != nil {
		return err
	}
	lockfile, err := os.ReadFile(lockfilePath)
	if err != nil {
		return fmt.Errorf("reading lockfile: %w", err)
	}
	if err := validateLockfile(cfg, lockfile); err != nil {
		return err
	}
	archiveType, err := archiveTypeOf(archivePath)
	if err != nil {
		return err
	}

	if writer.exists(writer.modulePath(cfg.Module, publishVersion)) {
		fmt.Fprintf(os.Stderr, "devregistry: %s %s is already published; replacing it\n", cfg.Module, publishVersion)
	}

	// Bazel resolves against the registry's copy of MODULE.bazel, and unpacks the
	// archive to get everything else. Patching both keeps the version the module
	// declares the same in either place.
	patchedModule, err := setModuleVersion(moduleContent, publishVersion)
	if err != nil {
		return err
	}
	patch, err := unifiedDiff(moduleBazel, moduleContent, patchedModule)
	if err != nil {
		return err
	}
	const patchName = "0001-set-module-version.patch"
	if err := writer.writeFile(writer.modulePath(cfg.Module, publishVersion, "patches", patchName), patch); err != nil {
		return fmt.Errorf("writing patch: %w", err)
	}
	if err := writer.writeFile(writer.modulePath(cfg.Module, publishVersion, moduleBazel), patchedModule); err != nil {
		return fmt.Errorf("writing %s: %w", moduleBazel, err)
	}

	// The lockfile is what makes this version fetch the tool from the artifact
	// built alongside it. It goes into the version's overlay, which Bazel layers
	// onto the extracted archive.
	lockfileName := cfg.Artifact.LockfileTitle
	if err := writer.writeFile(writer.modulePath(cfg.Module, publishVersion, overlayDir, lockfileName), lockfile); err != nil {
		return fmt.Errorf("writing overlay %s: %w", lockfileName, err)
	}

	assetPath, integrity, reused, err := writer.storeAsset(archivePath)
	if err != nil {
		return fmt.Errorf("storing source archive: %w", err)
	}
	if err := writer.writeJSON(writer.modulePath(cfg.Module, publishVersion, sourceJSON), sourceSpec{
		URL:         cfg.AssetBaseURL + "/" + strings.TrimPrefix(filepath.ToSlash(assetPath), assetsDir+"/"),
		Integrity:   integrity,
		ArchiveType: archiveType,
		Overlay:     map[string]string{lockfileName: sriOf(lockfile)},
		Patches:     map[string]string{patchName: sriOf(patch)},
		PatchStrip:  1,
	}); err != nil {
		return fmt.Errorf("writing %s: %w", sourceJSON, err)
	}

	var template []byte
	if cfg.MetadataTemplate != "" {
		if template, err = os.ReadFile(cfg.MetadataTemplate); err != nil {
			return fmt.Errorf("reading metadata template: %w", err)
		}
	}
	versions, err := writer.mergeMetadata(cfg.Module, publishVersion, template)
	if err != nil {
		return err
	}

	// A registry Bazel can read at all: an (empty) registry-level config, and the
	// marker that keeps the static site host from reinterpreting the tree.
	if !writer.exists(bazelRegistryJSON) {
		if err := writer.writeJSON(bazelRegistryJSON, struct{}{}); err != nil {
			return fmt.Errorf("writing %s: %w", bazelRegistryJSON, err)
		}
	}
	if err := writer.writeFile(noJekyll, nil); err != nil {
		return fmt.Errorf("writing %s: %w", noJekyll, err)
	}
	if err := writer.writeIndex(cfg, versions); err != nil {
		return fmt.Errorf("writing %s: %w", indexHTML, err)
	}

	stored := "stored a new source archive"
	if reused {
		stored = "reused the source archive already stored"
	}
	fmt.Fprintf(os.Stderr, "devregistry: published %s %s (%d versions total, %s)\n", cfg.Module, publishVersion, len(versions), stored)
	fmt.Fprintf(os.Stderr, "  common --registry=%s --registry=https://bcr.bazel.build\n", cfg.RegistryURL)
	fmt.Fprintf(os.Stderr, "  bazel_dep(name = %q, version = %q)\n", cfg.Module, publishVersion)
	return nil
}

// declaredVersion returns the version a MODULE.bazel declares.
func declaredVersion(moduleContent []byte) (string, error) {
	file, err := parseModuleBazel(moduleContent)
	if err != nil {
		return "", err
	}
	literal, err := moduleVersionLiteral(file)
	if err != nil {
		return "", err
	}
	return literal.Value, nil
}

// lockfileEntry is the part of a prebuilt lockfile entry this tool checks.
type lockfileEntry struct {
	DownloadMode string `json:"download_mode"`
	Registry     string `json:"registry"`
	Repository   string `json:"repository"`
	OS           string `json:"os"`
	CPU          string `json:"cpu"`
}

// validateLockfile checks that the prebuilt lockfile being published fetches the
// tool from the artifact this channel publishes alongside it.
//
// The lockfile is produced by a different workflow than the one publishing here,
// so the two can drift apart. A lockfile pointing at a repository nobody pushes
// to resolves for nobody, which is worth failing over before it is published.
func validateLockfile(cfg *Config, content []byte) error {
	name := cfg.Artifact.LockfileTitle
	var entries []lockfileEntry
	if err := json.Unmarshal(content, &entries); err != nil {
		return fmt.Errorf("parsing %s: %w", name, err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("%s is empty: the published module would have no prebuilt tool for any platform", name)
	}
	for _, entry := range entries {
		platform := entry.OS + "/" + entry.CPU
		if entry.DownloadMode != "oci" {
			return fmt.Errorf(
				"%s: entry for %s has download_mode %q, want \"oci\". A pre-release has no release assets to download from, so its lockfile has to name registry blobs",
				name, platform, entry.DownloadMode,
			)
		}
		if entry.Registry != cfg.Artifact.Registry || entry.Repository != cfg.Artifact.Repository {
			return fmt.Errorf(
				"%s: entry for %s fetches from %s/%s, but this channel publishes %s/%s",
				name, platform,
				entry.Registry, entry.Repository,
				cfg.Artifact.Registry, cfg.Artifact.Repository,
			)
		}
	}
	return nil
}

// checkArchiveShipsNoLockfile makes sure the archive carries no lockfile of its
// own.
//
// Archives are stored by content, and the published lockfile names blobs by
// digest, so an archive that had the real lockfile baked in would be a different
// archive for every commit -- which is the storage the overlay exists to avoid.
// An archive carrying an empty lockfile is what a build with
// `--//img/private/release:prebuilt_lockfile_override=//:prebuilt_lockfile.json`
// produces, and is what is expected here.
func checkArchiveShipsNoLockfile(cfg *Config, archivePath string) error {
	name := cfg.Artifact.LockfileTitle
	content, err := readFromArchive(archivePath, name)
	if errors.Is(err, errNotInArchive) {
		return nil
	}
	if err != nil {
		return err
	}
	var entries []lockfileEntry
	if err := json.Unmarshal(content, &entries); err != nil {
		return fmt.Errorf("parsing %s from the archive: %w", name, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf(
			"the archive ships a populated %s (%d entries), which would make it specific to this commit. Build it with an empty lockfile instead; the real one is published as this version's overlay",
			name, len(entries),
		)
	}
	return nil
}

// archiveTypeOf names an archive's format after its extension, for source.json to
// state: the stored archive is addressed by digest and so has no extension for
// Bazel to infer it from.
func archiveTypeOf(archivePath string) (string, error) {
	name := filepath.Base(archivePath)
	for _, archiveType := range []string{
		"tar.gz", "tgz", "tar.xz", "txz", "tar.zst", "tzst", "tar.bz2", "tar", "zip",
	} {
		if strings.HasSuffix(name, "."+archiveType) {
			return archiveType, nil
		}
	}
	return "", fmt.Errorf("cannot tell the archive type of %s from its name", name)
}
