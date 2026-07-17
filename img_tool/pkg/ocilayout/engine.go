package ocilayout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// fileEntry is a small in-memory file written verbatim.
type fileEntry struct {
	path string
	data []byte
}

// blobEntry is a content-addressed entry under blobs/sha256/.
type blobEntry struct {
	path string
	blob Blob
}

// plan is the fully-resolved set of things to write, order-independent except
// for the blob emission which is sorted by path.
type plan struct {
	marker     fileEntry
	rootFile   fileEntry  // index.json or root.descriptor.json
	dockerFile *fileEntry // manifest.json (optional)
	blobs      []blobEntry
	missing    []string
}

func blobPath(hex string) string { return "blobs/sha256/" + hex }

func marshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

func artifactTypeOf(m *v1.Manifest) string {
	// artifactType is only meaningful for non-image config media types (e.g.
	// Helm); it is omitted for standard image configs so index.json stays
	// stable and matches typical single-manifest OCI layouts.
	if m.Config.MediaType != "" && !m.Config.MediaType.IsConfig() {
		return string(m.Config.MediaType)
	}
	return ""
}

func readBlobAll(ctx context.Context, b Blob) ([]byte, error) {
	switch {
	case b.Bytes != nil:
		return b.Bytes, nil
	case b.Path != "":
		return os.ReadFile(b.Path)
	case b.Open != nil:
		rc, _, err := b.Open(ctx)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	default:
		return nil, errNoBlobContent
	}
}

func (b *Builder) emit(ctx context.Context, sink Sink) error {
	if err := b.format.Validate(); err != nil {
		return err
	}
	if b.format.needsRootIndex() && b.rootIndex == nil {
		return fmt.Errorf("ocilayout: index style requires SetRootIndex")
	}
	p, err := b.buildPlan(ctx)
	if err != nil {
		return err
	}
	if len(p.missing) > 0 {
		return &MissingBlobsError{MissingBlobs: p.missing, OutputGroup: b.missingHint}
	}
	return p.write(ctx, sink, b.writeOpts())
}

func (b *Builder) writeOpts() WriteBlobOptions {
	return WriteBlobOptions{UseSymlinks: b.useSymlinks, RequireLink: b.requireLink, ProgressFunc: b.progress}
}

func (b *Builder) buildPlan(ctx context.Context) (*plan, error) {
	p := &plan{}

	markerName, markerVer := b.format.markerFile()
	markerBytes, err := marshalIndent(map[string]string{"imageLayoutVersion": markerVer})
	if err != nil {
		return nil, err
	}
	p.marker = fileEntry{markerName, markerBytes}

	// Read the root index bytes for the index-backed styles.
	var rootBytes []byte
	indexMode := b.format.needsRootIndex() || (b.format.indexStyle == IndexRootDescriptor && b.rootIndex != nil)
	if indexMode {
		rootBytes, err = readBlobAll(ctx, *b.rootIndex)
		if err != nil {
			return nil, fmt.Errorf("reading root index: %w", err)
		}
	} else if len(b.manifests) == 0 {
		return nil, fmt.Errorf("ocilayout: no manifest provided")
	}

	// Which children are included, and which is the default (for manifest.json).
	included := []int{0}
	defaultIdx := 0
	if indexMode {
		included, defaultIdx, err = b.includedFromIndex(rootBytes)
		if err != nil {
			return nil, err
		}
	}

	switch b.format.indexStyle {
	case IndexClean:
		m := b.manifests[0]
		desc := v1.Descriptor{
			MediaType:   m.Manifest.MediaType,
			Digest:      hashBytes(m.ManifestData),
			Size:        int64(len(m.ManifestData)),
			Annotations: m.Manifest.Annotations,
		}
		if at := artifactTypeOf(m.Manifest); at != "" {
			desc.ArtifactType = at
		}
		data, err := marshalIndent(v1.IndexManifest{SchemaVersion: 2, MediaType: mediaTypeOCIImageIndex, Manifests: []v1.Descriptor{desc}})
		if err != nil {
			return nil, err
		}
		p.rootFile = fileEntry{"index.json", data}
		b.addManifestBlobs(p, m)

	case IndexAnnotated:
		m := b.manifests[0]
		descs := DescriptorsForTags(b.ociTags, m.Manifest.MediaType, m.ManifestData, hashBytes(m.ManifestData), artifactTypeOf(m.Manifest), b.format.tagOnly())
		data, err := marshalIndent(v1.IndexManifest{SchemaVersion: 2, MediaType: mediaTypeOCIImageIndex, Manifests: descs})
		if err != nil {
			return nil, err
		}
		p.rootFile = fileEntry{"index.json", data}
		b.addManifestBlobs(p, m)

	case IndexVerbatim:
		p.rootFile = fileEntry{"index.json", rootBytes}
		for _, i := range included {
			b.addManifestBlobs(p, b.manifests[i])
		}

	case IndexWrapping:
		indexDigest := hashBytes(rootBytes)
		descs := DescriptorsForTags(b.ociTags, types.OCIImageIndex, rootBytes, indexDigest, "", b.format.tagOnly())
		data, err := marshalIndent(v1.IndexManifest{SchemaVersion: 2, MediaType: mediaTypeOCIImageIndex, Manifests: descs})
		if err != nil {
			return nil, err
		}
		p.rootFile = fileEntry{"index.json", data}
		p.blobs = append(p.blobs, blobEntry{blobPath(indexDigest.Hex), BlobFromBytes(rootBytes)})
		for _, i := range included {
			b.addManifestBlobs(p, b.manifests[i])
		}

	case IndexRootDescriptor:
		if indexMode {
			indexDigest := hashBytes(rootBytes)
			var idx v1.IndexManifest
			if err := json.Unmarshal(rootBytes, &idx); err != nil {
				return nil, fmt.Errorf("parsing root index: %w", err)
			}
			data, err := marshalIndent(v1.Descriptor{MediaType: idx.MediaType, Digest: indexDigest, Size: int64(len(rootBytes))})
			if err != nil {
				return nil, err
			}
			p.rootFile = fileEntry{"root.descriptor.json", data}
			p.blobs = append(p.blobs, blobEntry{blobPath(indexDigest.Hex), BlobFromBytes(rootBytes)})
			for i := range b.manifests {
				b.addManifestBlobs(p, b.manifests[i])
			}
		} else {
			m := b.manifests[0]
			data, err := marshalIndent(v1.Descriptor{MediaType: m.Manifest.MediaType, Digest: hashBytes(m.ManifestData), Size: int64(len(m.ManifestData))})
			if err != nil {
				return nil, err
			}
			p.rootFile = fileEntry{"root.descriptor.json", data}
			b.addManifestBlobs(p, m)
		}

	default:
		return nil, fmt.Errorf("ocilayout: unknown index style %d", b.format.indexStyle)
	}

	if b.format.dockerManifest {
		def := b.manifests[0]
		if indexMode {
			def = b.manifests[defaultIdx]
		}
		data, err := dockerManifestBytes(def, b.repoTags)
		if err != nil {
			return nil, err
		}
		p.dockerFile = &fileEntry{"manifest.json", data}
	}

	return p, nil
}

// addManifestBlobs adds the manifest blob, config blob and layer blobs (or, for
// sparse layouts, the layer descriptor/cstream files) for one manifest.
func (b *Builder) addManifestBlobs(p *plan, m ManifestInput) {
	mfstDigest := hashBytes(m.ManifestData)
	p.blobs = append(p.blobs, blobEntry{blobPath(mfstDigest.Hex), BlobFromBytes(m.ManifestData)})
	p.blobs = append(p.blobs, blobEntry{blobPath(m.Manifest.Config.Digest.Hex), m.Config})

	for _, l := range m.Layers {
		hex := l.Descriptor.Digest.Hex
		if b.format.blobPolicy == BlobSparseLayers {
			descBytes, _ := marshalIndent(sparseLayerDescriptor(l))
			p.blobs = append(p.blobs, blobEntry{blobPath(hex) + ".descriptor.json", BlobFromBytes(descBytes)})
			if l.CompactStream != nil {
				p.blobs = append(p.blobs, blobEntry{blobPath(hex) + ".cstream", *l.CompactStream})
			}
			continue
		}
		if l.Present && !l.Blob.isZero() {
			p.blobs = append(p.blobs, blobEntry{blobPath(hex), l.Blob})
		} else if !b.allowMissing {
			p.missing = append(p.missing, l.Descriptor.Digest.String())
		}
	}
}

func sparseLayerDescriptor(l LayerInput) SparseLayerDescriptor {
	if l.SparseMeta != nil {
		return *l.SparseMeta
	}
	return SparseLayerDescriptor{
		MediaType:   string(l.Descriptor.MediaType),
		Digest:      l.Descriptor.Digest.String(),
		Size:        l.Descriptor.Size,
		Annotations: l.Descriptor.Annotations,
	}
}

func dockerManifestBytes(m ManifestInput, repoTags []string) ([]byte, error) {
	var layers []string
	for _, l := range m.Manifest.Layers {
		layers = append(layers, blobPath(l.Digest.Hex))
	}
	dm := dockerManifest{
		Config:   blobPath(m.Manifest.Config.Digest.Hex),
		RepoTags: repoTags,
		Layers:   layers,
	}
	return marshalIndent([]dockerManifest{dm})
}

// includedFromIndex applies the ManifestFilter (if any) to the root index's
// manifests, returning indices into the builder's manifest slice plus the
// default. Without a filter, all manifests are included and the first is
// default.
func (b *Builder) includedFromIndex(rootBytes []byte) ([]int, int, error) {
	var idx v1.IndexManifest
	if err := json.Unmarshal(rootBytes, &idx); err != nil {
		return nil, 0, fmt.Errorf("parsing root index: %w", err)
	}
	if b.filter != nil {
		descs := make([]ManifestDescriptor, len(idx.Manifests))
		for i, d := range idx.Manifests {
			descs[i] = ManifestDescriptor{Platform: d.Platform, Digest: d.Digest, Size: d.Size}
		}
		included, defaultIdx := b.filter(descs)
		if len(included) == 0 {
			return nil, 0, fmt.Errorf("ocilayout: ManifestFilter returned no manifests to include")
		}
		return included, defaultIdx, nil
	}
	included := make([]int, len(b.manifests))
	for i := range b.manifests {
		included[i] = i
	}
	return included, 0, nil
}

// write emits the plan to the sink in a deterministic order: blobs/ and
// blobs/sha256/ directory entries, the marker, the root file, an optional
// Docker manifest.json, then all blob entries deduplicated and sorted by path.
func (p *plan) write(ctx context.Context, sink Sink, opts WriteBlobOptions) error {
	if err := sink.CreateDir("blobs"); err != nil {
		return fmt.Errorf("creating blobs directory: %w", err)
	}
	if err := sink.CreateDir("blobs/sha256"); err != nil {
		return fmt.Errorf("creating blobs/sha256 directory: %w", err)
	}
	if err := sink.WriteFile(p.marker.path, p.marker.data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", p.marker.path, err)
	}
	if err := sink.WriteFile(p.rootFile.path, p.rootFile.data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", p.rootFile.path, err)
	}
	if p.dockerFile != nil {
		if err := sink.WriteFile(p.dockerFile.path, p.dockerFile.data, 0o644); err != nil {
			return fmt.Errorf("writing manifest.json: %w", err)
		}
	}

	seen := make(map[string]struct{}, len(p.blobs))
	unique := make([]blobEntry, 0, len(p.blobs))
	for _, e := range p.blobs {
		if _, ok := seen[e.path]; ok {
			continue
		}
		seen[e.path] = struct{}{}
		unique = append(unique, e)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i].path < unique[j].path })

	for _, e := range unique {
		if err := sink.WriteBlob(ctx, e.path, e.blob, opts); err != nil {
			return fmt.Errorf("writing blob %s: %w", e.path, err)
		}
	}
	return nil
}
