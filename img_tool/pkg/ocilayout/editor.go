package ocilayout

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Editor mutates an OCI image layout directory in place. Unlike the one-shot
// Builder it supports incremental additions:
//
//   - AddBlob writes a single blob idempotently (a blob that already exists is
//     a no-op).
//   - AddManifest adds a manifest, its config and layer blobs, and unions a
//     descriptor into index.json.
//
// index.json is rewritten atomically on Flush/Close. Because blobs are
// content-addressed and written before index.json, a crash mid-edit leaves at
// most orphan blobs, never a corrupt index.
type Editor struct {
	sink   *DirectorySink
	index  *v1.IndexManifest // nil when the root is not an index.json (e.g. sparse)
	format Format
	opts   WriteBlobOptions
	dirty  bool
}

// OpenDir opens an existing OCI layout directory for editing. It requires an
// "oci-layout" marker and reads index.json (if present).
func OpenDir(dir string) (*Editor, error) {
	sink := NewDirectorySink(dir)

	format := OCILayout()
	if _, err := os.Stat(filepath.Join(dir, "oci-layout")); err != nil {
		if _, serr := os.Stat(filepath.Join(dir, "sparse-oci-layout")); serr == nil {
			format = SparseOCILayout()
		} else {
			return nil, fmt.Errorf("ocilayout: %s is not an OCI layout (no oci-layout marker): %w", dir, err)
		}
	}

	e := &Editor{sink: sink, format: format}
	if data, err := sink.readFile("index.json"); err == nil {
		var idx v1.IndexManifest
		if err := json.Unmarshal(data, &idx); err != nil {
			return nil, fmt.Errorf("parsing index.json: %w", err)
		}
		e.index = &idx
	}
	return e, nil
}

// CreateDir creates a fresh OCI layout directory (marker + empty index.json)
// and returns an Editor for it. The format's marker selects oci-layout vs
// sparse-oci-layout.
func CreateDir(dir string, format Format) (*Editor, error) {
	sink := NewDirectorySink(dir)
	if err := sink.CreateDir("blobs/sha256"); err != nil {
		return nil, err
	}
	markerName, markerVer := format.markerFile()
	markerBytes, err := marshalIndent(map[string]string{"imageLayoutVersion": markerVer})
	if err != nil {
		return nil, err
	}
	if err := sink.WriteFile(markerName, markerBytes, 0o644); err != nil {
		return nil, err
	}

	e := &Editor{
		sink:   sink,
		format: format,
		index:  &v1.IndexManifest{SchemaVersion: 2, MediaType: mediaTypeOCIImageIndex},
		dirty:  true,
	}
	return e, e.Flush()
}

// WithLinkStrategy configures how Path blobs are materialized (see Builder).
func (e *Editor) WithLinkStrategy(useSymlinks, requireLink bool) *Editor {
	e.opts.UseSymlinks = useSymlinks
	e.opts.RequireLink = requireLink
	return e
}

// AddBlob writes blobs/sha256/<hex> from b. It is idempotent: if the blob
// already exists it is a no-op. index.json is not touched.
func (e *Editor) AddBlob(ctx context.Context, digest v1.Hash, b Blob) error {
	path := blobPath(digest.Hex)
	if e.sink.blobExists(path) {
		return nil
	}
	return e.sink.WriteBlob(ctx, path, b, e.opts)
}

// AddManifest adds the manifest, its config and layer blobs (each via the
// idempotent AddBlob) and unions a descriptor into index.json. When tags are
// given, one annotated descriptor per tag is added; otherwise a single clean
// descriptor. Re-adding the same manifest/tag is a no-op.
func (e *Editor) AddManifest(ctx context.Context, m ManifestInput, tags ...string) error {
	if e.index == nil {
		return fmt.Errorf("ocilayout: AddManifest requires an index.json root layout")
	}

	mfstDigest := hashBytes(m.ManifestData)
	if err := e.AddBlob(ctx, mfstDigest, BlobFromBytes(m.ManifestData)); err != nil {
		return fmt.Errorf("adding manifest blob: %w", err)
	}
	if err := e.AddBlob(ctx, m.Manifest.Config.Digest, m.Config); err != nil {
		return fmt.Errorf("adding config blob: %w", err)
	}
	for _, l := range m.Layers {
		if l.Blob.isZero() {
			continue
		}
		if err := e.AddBlob(ctx, l.Descriptor.Digest, l.Blob); err != nil {
			return fmt.Errorf("adding layer blob %s: %w", l.Descriptor.Digest, err)
		}
	}

	descs := DescriptorsForTags(tags, m.Manifest.MediaType, m.ManifestData, mfstDigest, artifactTypeOf(m.Manifest), e.format.tagOnly())
	for _, d := range descs {
		e.AddIndexEntry(d)
	}
	return nil
}

// AddIndexEntry unions a raw descriptor into index.json, deduplicating on
// (digest, org.opencontainers.image.ref.name) so repeated adds are no-ops.
func (e *Editor) AddIndexEntry(desc v1.Descriptor) error {
	if e.index == nil {
		return fmt.Errorf("ocilayout: AddIndexEntry requires an index.json root layout")
	}
	for _, existing := range e.index.Manifests {
		if existing.Digest == desc.Digest && refName(existing) == refName(desc) {
			return nil
		}
	}
	e.index.Manifests = append(e.index.Manifests, desc)
	e.dirty = true
	return nil
}

func refName(d v1.Descriptor) string {
	if d.Annotations == nil {
		return ""
	}
	return d.Annotations[api.AnnotationOCIImageRefName]
}

// Flush rewrites index.json atomically if there are pending changes.
func (e *Editor) Flush() error {
	if !e.dirty || e.index == nil {
		return nil
	}
	data, err := marshalIndent(e.index)
	if err != nil {
		return err
	}
	dir := e.sink.basePath
	tmp, err := os.CreateTemp(dir, ".index.json.*")
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
	if err := os.Rename(tmpName, filepath.Join(dir, "index.json")); err != nil {
		os.Remove(tmpName)
		return err
	}
	e.dirty = false
	return nil
}

// Close flushes any pending index.json changes.
func (e *Editor) Close() error { return e.Flush() }
