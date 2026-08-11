package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const testModuleBazel = `module(
    name = "rules_img",
    version = "0.3.19",
)

bazel_dep(name = "bazel_skylib", version = "1.9.0")

# A dependency whose own version = must not be mistaken for the module's.
bazel_dep(name = "platforms", version = "1.0.0")
`

const testLockfile = `[
  {
    "download_mode": "oci",
    "sha256": "f3993de5fc7b88a93d4e442a3444a784e095dd59172a30d19fd3ea3c97183ce2",
    "registry": "ghcr.io",
    "repository": "bazel-contrib/rules_img/img",
    "os": "linux",
    "cpu": "amd64"
  }
]
`

func testConfig(t *testing.T) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dev-registry.json")
	content := `{
  "module": "rules_img",
  "registry_url": "https://example.github.io/rules_img/",
  "pages_branch": "pages",
  "artifact": {
    "registry": "ghcr.io",
    "repository": "bazel-contrib/rules_img/img"
  }
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// writeArchive builds a .tar.gz holding the given files, the way a release build
// packages a module's source.
func writeArchive(t *testing.T, files map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "module.tar.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	compressor := gzip.NewWriter(file)
	writer := tar.NewWriter(compressor)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := files[name]
		// rules_pkg spells entry names with a leading "./".
		if err := writer.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg := testConfig(t)
	if got, want := cfg.RegistryURL, "https://example.github.io/rules_img"; got != want {
		t.Errorf("RegistryURL = %q, want %q (trailing slash trimmed)", got, want)
	}
	if got, want := cfg.AssetBaseURL, "https://example.github.io/rules_img/assets"; got != want {
		t.Errorf("AssetBaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.Artifact.LockfileTitle, "prebuilt_lockfile.json"; got != want {
		t.Errorf("LockfileTitle = %q, want %q", got, want)
	}
}

func TestLoadConfigRejectsIncomplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev-registry.json")
	if err := os.WriteFile(path, []byte(`{"module": "rules_img"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected an error for a config without a registry URL or artifact")
	}
	for _, want := range []string{"registry_url", "artifact.registry", "artifact.repository"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestReadFromArchive(t *testing.T) {
	archive := writeArchive(t, map[string]string{
		"MODULE.bazel":           testModuleBazel,
		"prebuilt_lockfile.json": testLockfile,
	})
	content, err := readFromArchive(archive, "MODULE.bazel")
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != testModuleBazel {
		t.Error("MODULE.bazel read back from the archive does not match")
	}
	if _, err := readFromArchive(archive, "nonexistent"); err == nil {
		t.Error("expected an error for a file the archive does not contain")
	}
}

func TestDeclaredVersion(t *testing.T) {
	got, err := declaredVersion([]byte(testModuleBazel))
	if err != nil {
		t.Fatal(err)
	}
	if want := "0.3.19"; got != want {
		t.Errorf("declaredVersion = %q, want %q", got, want)
	}
}

func TestValidateLockfile(t *testing.T) {
	cfg := testConfig(t)

	for name, tc := range map[string]struct {
		lockfile string
		wantErr  string
	}{
		"oci entry matching the channel": {
			lockfile: testLockfile,
		},
		"url mode": {
			lockfile: `[{"version": "v0.3.19", "integrity": "sha256-x", "os": "linux", "cpu": "amd64"}]`,
			wantErr:  "download_mode",
		},
		"other repository": {
			lockfile: `[{"download_mode": "oci", "registry": "ghcr.io", "repository": "someone/else", "os": "linux", "cpu": "amd64"}]`,
			wantErr:  "someone/else",
		},
		"empty": {
			lockfile: `[]`,
			wantErr:  "is empty",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateLockfile(cfg, []byte(tc.lockfile))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// publishInto runs the tool's publish step against a registry directory, with the
// lockfile published as an overlay rather than packaged into the archive.
func publishInto(t *testing.T, cfg *Config, root, version string, files map[string]string) error {
	t.Helper()
	return publishWithLockfile(t, cfg, root, version, files, testLockfile)
}

func publishWithLockfile(t *testing.T, cfg *Config, root, version string, files map[string]string, lockfile string) error {
	t.Helper()
	lockfilePath := filepath.Join(t.TempDir(), "prebuilt_lockfile.json")
	if err := os.WriteFile(lockfilePath, []byte(lockfile), 0o644); err != nil {
		t.Fatal(err)
	}
	return publish(cfg, &registryWriter{root: root}, version, writeArchive(t, files), lockfilePath)
}

// defaultArchiveFiles is what a release build packages for a pre-release: an
// empty lockfile, so the archive is the same for every commit that leaves the
// sources alone.
func defaultArchiveFiles() map[string]string {
	return map[string]string{
		"MODULE.bazel":           testModuleBazel,
		"prebuilt_lockfile.json": "[]\n",
		"README.md":              "# rules_img\n",
	}
}

func TestPublish(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	const version = "0.3.20-20260811-fa8b7de"

	if err := publishInto(t, cfg, root, version, defaultArchiveFiles()); err != nil {
		t.Fatal(err)
	}

	// The registry copy of MODULE.bazel declares the published version, and is
	// otherwise the archive's.
	patched, err := os.ReadFile(filepath.Join(root, "modules/rules_img", version, "MODULE.bazel"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patched), `version = "`+version+`"`) {
		t.Errorf("registry MODULE.bazel does not declare %q:\n%s", version, patched)
	}
	if !strings.Contains(string(patched), `bazel_dep(name = "platforms", version = "1.0.0")`) {
		t.Error("patching the module version rewrote unrelated lines")
	}

	// source.json points at the archive next to it, pinned by integrity.
	var source sourceSpec
	sourceContent, err := os.ReadFile(filepath.Join(root, "modules/rules_img", version, "source.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(sourceContent, &source); err != nil {
		t.Fatal(err)
	}
	// The archive is addressed by content, under a name that carries no extension,
	// so source.json has to state the archive type.
	assetURL, ok := strings.CutPrefix(source.URL, cfg.AssetBaseURL+"/")
	if !ok {
		t.Fatalf("source.json url = %q, want it under %q", source.URL, cfg.AssetBaseURL)
	}
	asset, err := os.ReadFile(filepath.Join(root, "assets", assetURL))
	if err != nil {
		t.Fatalf("source archive was not stored in the assets: %v", err)
	}
	if got := sriOf(asset); got != source.Integrity {
		t.Errorf("source.json integrity = %q, but the asset hashes to %q", source.Integrity, got)
	}
	digest := sha256.Sum256(asset)
	if want := "sha256/" + hex.EncodeToString(digest[:]) + "/file"; assetURL != want {
		t.Errorf("archive stored at %q, want the content addressed %q", assetURL, want)
	}
	if source.ArchiveType != "tar.gz" {
		t.Errorf("source.json archive_type = %q, want \"tar.gz\"", source.ArchiveType)
	}
	if source.PatchStrip != 1 || len(source.Patches) != 1 {
		t.Errorf("source.json should carry exactly one patch applied with strip 1, got %+v", source)
	}

	// The lockfile is layered on per version rather than packaged into the archive.
	overlay, err := os.ReadFile(filepath.Join(root, "modules/rules_img", version, "overlay/prebuilt_lockfile.json"))
	if err != nil {
		t.Fatalf("lockfile was not published as an overlay: %v", err)
	}
	if string(overlay) != testLockfile {
		t.Errorf("overlaid lockfile:\n%s\nwant:\n%s", overlay, testLockfile)
	}
	if got, want := source.Overlay["prebuilt_lockfile.json"], sriOf(overlay); got != want {
		t.Errorf("source.json overlay records %q for the lockfile, but it hashes to %q", got, want)
	}
	archived, err := readFromArchive(filepath.Join(root, "assets", assetURL), "prebuilt_lockfile.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(archived)) != "[]" {
		t.Errorf("the stored archive ships a lockfile of its own: %s", archived)
	}

	// The recorded patch is the one on disk, and applying it to the archive's
	// MODULE.bazel yields the registry copy.
	for name, integrity := range source.Patches {
		patch, err := os.ReadFile(filepath.Join(root, "modules/rules_img", version, "patches", name))
		if err != nil {
			t.Fatal(err)
		}
		if got := sriOf(patch); got != integrity {
			t.Errorf("source.json records %q for %s, but it hashes to %q", integrity, name, got)
		}
		applied, err := applyUnifiedDiff([]byte(testModuleBazel), patch)
		if err != nil {
			t.Fatalf("applying %s: %v", name, err)
		}
		if string(applied) != string(patched) {
			t.Errorf("applying %s does not produce the registry MODULE.bazel", name)
		}
	}

	for _, path := range []string{"bazel_registry.json", ".nojekyll", "index.html"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("registry is missing %s: %v", path, err)
		}
	}
	index, err := os.ReadFile(filepath.Join(root, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{version, cfg.RegistryURL, "2026-08-11", "fa8b7de"} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.html does not mention %q", want)
		}
	}
}

func TestPublishIsAppendOnly(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()
	versions := []string{"0.3.20-20260810-aaaaaaa", "0.3.20-20260811-bbbbbbb", "0.4.0-20260812-ccccccc"}
	for _, version := range versions {
		if err := publishInto(t, cfg, root, version, defaultArchiveFiles()); err != nil {
			t.Fatalf("publishing %s: %v", version, err)
		}
	}

	// Every version ever published is still resolvable.
	for _, version := range versions {
		for _, name := range []string{"MODULE.bazel", "source.json"} {
			if _, err := os.Stat(filepath.Join(root, "modules/rules_img", version, name)); err != nil {
				t.Errorf("%s of %s went missing: %v", name, version, err)
			}
		}
	}

	var metadata struct {
		Versions       []string       `json:"versions"`
		Homepage       string         `json:"homepage"`
		YankedVersions map[string]any `json:"yanked_versions"`
	}
	content, err := os.ReadFile(filepath.Join(root, "modules/rules_img/metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &metadata); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(metadata.Versions, ","), strings.Join(versions, ","); got != want {
		t.Errorf("metadata.json versions = %q, want %q (oldest first, all of them)", got, want)
	}

	// Republishing a version neither duplicates it nor drops the others.
	if err := publishInto(t, cfg, root, versions[0], defaultArchiveFiles()); err != nil {
		t.Fatal(err)
	}
	content, err = os.ReadFile(filepath.Join(root, "modules/rules_img/metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	metadata.Versions = nil
	if err := json.Unmarshal(content, &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Versions) != len(versions) {
		t.Errorf("republishing changed the version list to %v", metadata.Versions)
	}

	// The newest version leads the landing page.
	index, err := os.ReadFile(filepath.Join(root, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `version = "0.4.0-20260812-ccccccc"`) {
		t.Error("index.html does not offer the newest version in its usage snippet")
	}
}

func TestPublishSeedsMetadataFromTemplate(t *testing.T) {
	cfg := testConfig(t)
	template := filepath.Join(t.TempDir(), "metadata.template.json")
	if err := os.WriteFile(template, []byte(`{
  "homepage": "https://github.com/bazel-contrib/rules_img",
  "repository": ["github:bazel-contrib/rules_img"],
  "versions": [],
  "yanked_versions": {}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.MetadataTemplate = template

	root := t.TempDir()
	if err := publishInto(t, cfg, root, "0.3.20-20260811-fa8b7de", defaultArchiveFiles()); err != nil {
		t.Fatal(err)
	}

	var metadata struct {
		Homepage   string   `json:"homepage"`
		Repository []string `json:"repository"`
		Versions   []string `json:"versions"`
	}
	content, err := os.ReadFile(filepath.Join(root, "modules/rules_img/metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(content, &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Homepage == "" || len(metadata.Repository) != 1 {
		t.Errorf("metadata.json did not keep the template's fields: %+v", metadata)
	}
	if len(metadata.Versions) != 1 {
		t.Errorf("metadata.json versions = %v", metadata.Versions)
	}

	// With a repository known, versions link to the commit they were built from.
	index, err := os.ReadFile(filepath.Join(root, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "https://github.com/bazel-contrib/rules_img/commit/fa8b7de") {
		t.Error("index.html does not link a version to its commit")
	}
}

// countStoredArchives returns how many archives the content addressed store holds.
func countStoredArchives(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "assets", "sha256"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(entries)
}

func TestPublishStoresArchivesByContent(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()

	// A commit that leaves the module's sources alone publishes the same archive,
	// which must not be stored a second time -- only its version's metadata is new.
	if err := publishInto(t, cfg, root, "0.3.20-20260810-aaaaaaa", defaultArchiveFiles()); err != nil {
		t.Fatal(err)
	}
	if err := publishInto(t, cfg, root, "0.3.20-20260811-bbbbbbb", defaultArchiveFiles()); err != nil {
		t.Fatal(err)
	}
	if got := countStoredArchives(t, root); got != 1 {
		t.Errorf("two versions of identical sources stored %d archives, want 1", got)
	}

	// A commit that does change them stores a second archive, and the older
	// version keeps pointing at the older one.
	changed := defaultArchiveFiles()
	changed["README.md"] = "# rules_img, now with a changed README\n"
	if err := publishInto(t, cfg, root, "0.3.20-20260812-ccccccc", changed); err != nil {
		t.Fatal(err)
	}
	if got := countStoredArchives(t, root); got != 2 {
		t.Errorf("a changed source tree stored %d archives in total, want 2", got)
	}

	urls := map[string]string{}
	for _, version := range []string{"0.3.20-20260810-aaaaaaa", "0.3.20-20260811-bbbbbbb", "0.3.20-20260812-ccccccc"} {
		var source sourceSpec
		content, err := os.ReadFile(filepath.Join(root, "modules/rules_img", version, "source.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(content, &source); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(source.URL, "/file") {
			t.Errorf("%s: url = %q, want a content addressed path", version, source.URL)
		}
		urls[version] = source.URL
	}
	if urls["0.3.20-20260810-aaaaaaa"] != urls["0.3.20-20260811-bbbbbbb"] {
		t.Error("versions with identical sources point at different archives")
	}
	if urls["0.3.20-20260811-bbbbbbb"] == urls["0.3.20-20260812-ccccccc"] {
		t.Error("a changed source tree points at the same archive as before")
	}
}

func TestPublishRejectsArchiveCarryingALockfile(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()

	// An archive with the real lockfile baked in would differ for every commit,
	// defeating the content addressed store the overlay exists to enable.
	files := defaultArchiveFiles()
	files["prebuilt_lockfile.json"] = testLockfile
	err := publishInto(t, cfg, root, "0.3.20-20260811-fa8b7de", files)
	if err == nil {
		t.Fatal("expected an error for an archive that ships a populated lockfile")
	}
	if !strings.Contains(err.Error(), "specific to this commit") {
		t.Errorf("error %q does not explain the problem", err)
	}
}

func TestPublishRejectsVersionBelowTheRelease(t *testing.T) {
	cfg := testConfig(t)
	root := t.TempDir()

	// A prerelease of the version that is already released loses against it in
	// module resolution, so it must not be published.
	err := publishInto(t, cfg, root, "0.3.19-20260811-fa8b7de", defaultArchiveFiles())
	if err == nil {
		t.Fatal("expected an error for a prerelease of the released version")
	}
	if !strings.Contains(err.Error(), "0.3.20") {
		t.Errorf("error %q should suggest the next version up", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "modules")); statErr == nil {
		t.Error("nothing should have been written")
	}
}
