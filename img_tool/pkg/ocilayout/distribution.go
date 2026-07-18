package ocilayout

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// DistributionWriter materializes images into a static, read-only container
// registry layout that mirrors the OCI distribution-spec paths a webserver
// would serve for parameter-less GETs:
//
//	<root>/<name>/blobs/sha256:<hex>          config + layer blobs
//	<root>/<name>/manifests/sha256:<hex>      manifests/indexes, stored once per digest
//	<root>/<name>/manifests/<tag>             same-dir relative symlink -> sha256:<hex>
//	<root>/<name>/tags/list                   generated {"name","tags":[...]}
//	<root>/<name>/referrers/sha256:<subject>  generated referrers index
//
// The <root> is <dir>/<registry>/v2 (per-registry split) or <dir>/v2 (flat,
// registry stripped). It is safe to write into a fresh, non-existent directory
// or one that already holds content: blobs and manifests are content-addressed
// and written idempotently, and tags/list + referrers are (re)generated from the
// on-disk state at Close, so pre-existing content is preserved and merged.
//
// A DistributionWriter is NOT safe for concurrent use; callers that share one
// across goroutines (e.g. the deploy persistent worker) must serialize access.
type DistributionWriter struct {
	root string
	flat bool
	opts WriteBlobOptions

	// blobPaths maps a blob hex to the first on-disk file written for it, so a
	// later repository can hardlink (or copy) instead of re-streaming.
	blobPaths map[string]string
	// touched records every repository root written to, so Close knows which
	// tags/list and referrers files to regenerate.
	touched map[string]DistributionRef
}

// DistributionRef identifies one repository within the layout. Name is the
// repository (e.g. "library/busybox"); Registry is the host component used for
// the per-registry split and ignored in flat mode.
type DistributionRef struct {
	Registry string
	Name     string
}

// DistributionImage is one root (image manifest or index) to write under a
// repository, together with its concrete child image manifests and the bare
// tags that should point at the root.
type DistributionImage struct {
	Ref DistributionRef
	// RootData is the exact raw bytes of the root object (manifest or index).
	RootData []byte
	// Children are the concrete image manifests whose config/layer blobs must
	// be written. For a single-manifest root this holds that one manifest (its
	// bytes equal RootData).
	Children []ManifestInput
	// Tags are bare tags (e.g. "latest") symlinked to the root digest.
	Tags []string
}

// NewDistributionWriter returns a writer rooted at dir. When flat is true the
// registry component is stripped (a single v2/ tree); otherwise each registry
// gets its own <registry>/v2 subtree.
func NewDistributionWriter(dir string, flat bool) *DistributionWriter {
	return &DistributionWriter{
		root:      dir,
		flat:      flat,
		blobPaths: make(map[string]string),
		touched:   make(map[string]DistributionRef),
	}
}

// WithLinkStrategy configures how blobs shared between repositories are
// materialized (see Builder.WithLinkStrategy).
func (w *DistributionWriter) WithLinkStrategy(useSymlinks, requireLink bool) *DistributionWriter {
	w.opts.UseSymlinks = useSymlinks
	w.opts.RequireLink = requireLink
	return w
}

// AddImage writes one root's blobs and manifests under img.Ref and records its
// tags. Blobs and manifests already present on disk are left untouched.
func (w *DistributionWriter) AddImage(ctx context.Context, img DistributionImage) error {
	repoRoot := w.repoRoot(img.Ref)
	w.touched[repoRoot] = img.Ref

	for i := range img.Children {
		child := img.Children[i]
		if !child.Config.isZero() {
			if err := w.putBlob(ctx, repoRoot, child.Manifest.Config.Digest.Hex, child.Config); err != nil {
				return fmt.Errorf("writing config blob: %w", err)
			}
		}
		for _, l := range child.Layers {
			if l.Blob.isZero() {
				continue
			}
			if err := w.putBlob(ctx, repoRoot, l.Descriptor.Digest.Hex, l.Blob); err != nil {
				return fmt.Errorf("writing layer blob %s: %w", l.Descriptor.Digest, err)
			}
		}
		if _, err := w.putManifest(repoRoot, child.ManifestData); err != nil {
			return fmt.Errorf("writing child manifest: %w", err)
		}
	}

	rootHash, err := w.putManifest(repoRoot, img.RootData)
	if err != nil {
		return fmt.Errorf("writing root manifest: %w", err)
	}
	for _, tag := range img.Tags {
		if err := w.putTag(repoRoot, tag, rootHash); err != nil {
			return fmt.Errorf("tagging %q: %w", tag, err)
		}
	}
	return nil
}

// Close regenerates tags/list and referrers for every repository written to.
func (w *DistributionWriter) Close() error {
	for repoRoot, ref := range w.touched {
		if err := w.writeTagsList(repoRoot, ref.Name); err != nil {
			return fmt.Errorf("writing tags/list for %s: %w", ref.Name, err)
		}
		if err := w.writeReferrers(repoRoot); err != nil {
			return fmt.Errorf("writing referrers for %s: %w", ref.Name, err)
		}
	}
	return nil
}

func (w *DistributionWriter) repoRoot(ref DistributionRef) string {
	name := filepath.FromSlash(ref.Name)
	if w.flat {
		return filepath.Join(w.root, "v2", name)
	}
	return filepath.Join(w.root, ref.Registry, "v2", name)
}

// putBlob writes blobs/sha256:<hex> under repoRoot, hardlinking (or copying)
// from an already-materialized copy of the same blob when possible. It is a
// no-op if the destination already exists.
func (w *DistributionWriter) putBlob(ctx context.Context, repoRoot, hex string, b Blob) error {
	rel := "blobs/sha256:" + hex
	dst := filepath.Join(repoRoot, filepath.FromSlash(rel))
	if fileExists(dst) {
		if _, ok := w.blobPaths[hex]; !ok {
			w.blobPaths[hex] = dst
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if first, ok := w.blobPaths[hex]; ok {
		if err := copyFile(first, dst, w.opts.UseSymlinks); err == nil {
			return nil
		}
		// Linking failed (e.g. cross-device); fall back to a full write below.
	}
	sink := &DirectorySink{basePath: repoRoot}
	if err := sink.WriteBlob(ctx, rel, b, w.opts); err != nil {
		return err
	}
	w.blobPaths[hex] = dst
	return nil
}

// putManifest stores data once at manifests/sha256:<hex> and returns its digest.
func (w *DistributionWriter) putManifest(repoRoot string, data []byte) (v1.Hash, error) {
	h := hashBytes(data)
	dst := filepath.Join(repoRoot, "manifests", "sha256:"+h.Hex)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return v1.Hash{}, err
	}
	if !fileExists(dst) {
		if err := os.WriteFile(dst, data, blobMode); err != nil {
			return v1.Hash{}, err
		}
	}
	return h, nil
}

// putTag creates a same-dir relative symlink manifests/<tag> -> sha256:<hex>.
func (w *DistributionWriter) putTag(repoRoot, tag string, h v1.Hash) error {
	if tag == "" {
		return nil
	}
	mdir := filepath.Join(repoRoot, "manifests")
	if err := os.MkdirAll(mdir, 0o755); err != nil {
		return err
	}
	link := filepath.Join(mdir, tag)
	_ = os.Remove(link) // idempotent re-tag
	return os.Symlink("sha256:"+h.Hex, link)
}

// writeTagsList regenerates tags/list from the tag symlinks present in the
// manifests directory (every entry whose name is not a digest).
func (w *DistributionWriter) writeTagsList(repoRoot, name string) error {
	mdir := filepath.Join(repoRoot, "manifests")
	entries, err := os.ReadDir(mdir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	tags := []string{}
	for _, e := range entries {
		if strings.Contains(e.Name(), ":") {
			continue // a sha256:<hex> manifest file, not a tag
		}
		tags = append(tags, e.Name())
	}
	sort.Strings(tags)
	data, err := marshalIndent(struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}{Name: name, Tags: tags})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(repoRoot, "tags"), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(repoRoot, "tags", "list"), data)
}

// referrerManifest is the subset of a manifest inspected to build a referrers
// listing.
type referrerManifest struct {
	MediaType    string `json:"mediaType"`
	ArtifactType string `json:"artifactType"`
	Config       struct {
		MediaType string `json:"mediaType"`
	} `json:"config"`
	Subject *v1.Descriptor `json:"subject"`
}

// writeReferrers (re)generates referrers/sha256:<subject> for every subject
// referenced by a manifest under repoRoot, following the distribution-spec
// "listing referrers" format (an OCI image index of referrer descriptors).
func (w *DistributionWriter) writeReferrers(repoRoot string) error {
	mdir := filepath.Join(repoRoot, "manifests")
	entries, err := os.ReadDir(mdir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	bySubject := make(map[string][]v1.Descriptor)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "sha256:") {
			continue // tag symlink, not a manifest
		}
		data, err := os.ReadFile(filepath.Join(mdir, e.Name()))
		if err != nil {
			return err
		}
		var m referrerManifest
		if json.Unmarshal(data, &m) != nil || m.Subject == nil {
			continue
		}
		artifactType := m.ArtifactType
		if artifactType == "" {
			artifactType = m.Config.MediaType
		}
		desc := v1.Descriptor{
			MediaType:    types.MediaType(m.MediaType),
			Size:         int64(len(data)),
			Digest:       hashBytes(data),
			ArtifactType: artifactType,
		}
		subject := m.Subject.Digest.String()
		bySubject[subject] = append(bySubject[subject], desc)
	}
	if len(bySubject) == 0 {
		return nil
	}

	rdir := filepath.Join(repoRoot, "referrers")
	if err := os.MkdirAll(rdir, 0o755); err != nil {
		return err
	}
	for subject, descs := range bySubject {
		sort.Slice(descs, func(i, j int) bool { return descs[i].Digest.String() < descs[j].Digest.String() })
		index := v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: descs}
		data, err := marshalIndent(index)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(filepath.Join(rdir, subject), data); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeFileAtomic writes data to path via a temp file + rename in the same dir.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
