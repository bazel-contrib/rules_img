package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A Bazel registry is a directory tree Bazel reads over plain HTTP: a module's
// MODULE.bazel and source.json per version, its list of versions, and -- because
// this registry hosts the archives it points at -- the archives themselves.
const (
	bazelRegistryJSON = "bazel_registry.json"
	metadataJSON      = "metadata.json"
	moduleBazel       = "MODULE.bazel"
	sourceJSON        = "source.json"
	indexHTML         = "index.html"

	// GitHub Pages runs Jekyll over a branch unless told not to, which would
	// drop paths it considers special.
	noJekyll = ".nojekyll"

	// Assets are content addressed, under
	// assets/sha256/<digest>/<assetFileStem>.<archive type>.
	//
	// The extension is not decoration: GitHub Pages serves a file it cannot type
	// as application/octet-stream and gzips *that* on the wire for any client
	// offering gzip -- which is every Bazel -- so the bytes arriving are a gzip of
	// the archive and no longer match the checksum the registry recorded. Named
	// with its extension, the same file is served as application/gzip and passed
	// through untouched.
	assetsDir     = "assets"
	assetFileStem = "file"
	overlayDir    = "overlay"
)

// sourceSpec is a version's source.json: where Bazel fetches the module from and
// what to do to it after unpacking.
type sourceSpec struct {
	URL         string            `json:"url"`
	Integrity   string            `json:"integrity"`
	StripPrefix string            `json:"strip_prefix,omitempty"`
	ArchiveType string            `json:"archive_type,omitempty"`
	Overlay     map[string]string `json:"overlay,omitempty"`
	Patches     map[string]string `json:"patches,omitempty"`
	PatchStrip  int               `json:"patch_strip,omitempty"`
}

// registryWriter writes into a checkout of the branch the registry is served
// from. It only ever adds or replaces the files of the version being published:
// a registry is append-only, because a version somebody already resolved has to
// keep resolving.
type registryWriter struct {
	root string
}

func (w *registryWriter) modulePath(module, version string, elem ...string) string {
	return filepath.Join(append([]string{"modules", module, version}, elem...)...)
}

func (w *registryWriter) writeFile(relPath string, content []byte) error {
	absPath := filepath.Join(w.root, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(absPath, content, 0o644)
}

func (w *registryWriter) writeJSON(relPath string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return w.writeFile(relPath, append(content, '\n'))
}

func (w *registryWriter) readFile(relPath string) ([]byte, error) {
	return os.ReadFile(filepath.Join(w.root, relPath))
}

func (w *registryWriter) exists(relPath string) bool {
	_, err := os.Stat(filepath.Join(w.root, relPath))
	return err == nil
}

// sriOf returns the Subresource Integrity of some content, the form a registry
// records checksums in.
func sriOf(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256-" + base64.StdEncoding.EncodeToString(digest[:])
}

// storeAsset adds a file to the registry's content addressed assets under
// fileName, and returns where it landed relative to the registry root, its
// Subresource Integrity, and whether it was already there.
//
// Addressing assets by content rather than by version is what keeps the registry
// from growing a copy of the same archive per commit: only commits that change
// the module's sources produce an archive that is not already stored.
func (w *registryWriter) storeAsset(srcPath, fileName string) (relPath, integrity string, reused bool, err error) {
	source, err := os.Open(srcPath)
	if err != nil {
		return "", "", false, err
	}
	defer source.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, source); err != nil {
		return "", "", false, err
	}
	digest := hash.Sum(nil)
	relPath = filepath.Join(assetsDir, "sha256", hex.EncodeToString(digest), fileName)
	integrity = "sha256-" + base64.StdEncoding.EncodeToString(digest)
	if w.exists(relPath) {
		return relPath, integrity, true, nil
	}

	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return "", "", false, err
	}
	absPath := filepath.Join(w.root, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return "", "", false, err
	}
	destination, err := os.Create(absPath)
	if err != nil {
		return "", "", false, err
	}
	defer destination.Close()
	if _, err := io.Copy(destination, source); err != nil {
		return "", "", false, err
	}
	if err := destination.Close(); err != nil {
		return "", "", false, err
	}
	return relPath, integrity, false, nil
}

// mergeMetadata adds a version to a module's metadata.json and returns every
// version the file now lists, newest first.
//
// Versions are append-only in publish order (oldest first in the file). Sorting
// by version string would put same-day builds in hash order rather than the
// order they hit main; the landing page instead lists the reverse, with the
// most recently published version marked latest.
//
// The file is merged rather than rewritten: it carries the module's homepage and
// maintainers (seeded from a template the first time), it accumulates every
// version ever published, and a maintainer may have yanked one. Fields this tool
// does not know about are preserved.
func (w *registryWriter) mergeMetadata(module, publish string, template []byte) ([]string, error) {
	relPath := filepath.Join("modules", module, metadataJSON)

	metadata := map[string]any{}
	existing, err := w.readFile(relPath)
	if err == nil {
		if err := json.Unmarshal(existing, &metadata); err != nil {
			return nil, fmt.Errorf("parsing existing %s: %w", relPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", relPath, err)
	} else if len(template) > 0 {
		if err := json.Unmarshal(template, &metadata); err != nil {
			return nil, fmt.Errorf("parsing metadata template: %w", err)
		}
	}

	published := map[string]bool{}
	var versions []string
	if listed, ok := metadata["versions"].([]any); ok {
		for _, entry := range listed {
			value, ok := entry.(string)
			if !ok {
				return nil, fmt.Errorf("%s: versions contains a non-string entry", relPath)
			}
			if published[value] {
				continue
			}
			published[value] = true
			versions = append(versions, value)
		}
	}
	if !published[publish] {
		versions = append(versions, publish)
	}

	metadata["versions"] = versions
	if _, ok := metadata["yanked_versions"]; !ok {
		metadata["yanked_versions"] = map[string]any{}
	}
	if err := w.writeJSON(relPath, metadata); err != nil {
		return nil, fmt.Errorf("writing %s: %w", relPath, err)
	}

	newestFirst := append([]string{}, versions...)
	reverse(newestFirst)
	return newestFirst, nil
}

func reverse(values []string) {
	for i, j := 0, len(values)-1; i < j; i, j = i+1, j-1 {
		values[i], values[j] = values[j], values[i]
	}
}

// readMetadataRepository returns the first `github:owner/repo` entry of a
// module's metadata, used to link a published version to the commit it was built
// from.
func (w *registryWriter) readMetadataRepository(module string) string {
	content, err := w.readFile(filepath.Join("modules", module, metadataJSON))
	if err != nil {
		return ""
	}
	var metadata struct {
		Repository []string `json:"repository"`
	}
	if err := json.Unmarshal(content, &metadata); err != nil {
		return ""
	}
	for _, entry := range metadata.Repository {
		if repo, ok := strings.CutPrefix(entry, "github:"); ok {
			return "https://github.com/" + repo
		}
	}
	return ""
}
