package ocilayout

import (
	"context"
	"io"
	"os"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// ManifestInput is everything the builder needs about one image manifest,
// described source-agnostically. It supersedes the per-command blobMap entries
// and the old ocitar.ManifestInfo.
type ManifestInput struct {
	// Manifest is the parsed manifest, used to derive config/layer descriptors
	// and the Docker manifest.json. It is never used for blob bytes.
	Manifest *v1.Manifest
	// ManifestData is the exact raw manifest bytes: hashed for the digest and
	// written as the manifest blob. Never re-marshaled.
	ManifestData []byte
	// Config is the config blob source.
	Config Blob
	// Layers are the layer blob sources, in manifest order.
	Layers []LayerInput
	// Platform is used by a ManifestFilter for index styles.
	Platform *v1.Platform
}

// LayerInput describes one layer's descriptor and blob source. A single input
// covers the full, shallow (missing) and sparse cases.
type LayerInput struct {
	// Descriptor is the layer descriptor as it appears in the manifest.
	Descriptor v1.Descriptor
	// Blob is the layer content. A zero Blob means the body is not available.
	Blob Blob
	// Present reports whether the body is available. When false under
	// BlobIncludeAll, the digest is reported as missing (unless allowMissing);
	// under BlobSparseLayers the body is intentionally omitted regardless.
	Present bool
	// CompactStream, when set (sparse), is written as <hex>.cstream.
	CompactStream *Blob
	// SparseMeta, when set (sparse), overrides the generated
	// <hex>.descriptor.json content.
	SparseMeta *SparseLayerDescriptor
}

// ManifestInputFromVFS builds a ManifestInput whose config and layer blobs
// stream from src (typically a VFS-backed BlobSource). It is shared by the load
// pipeline and the deploy sinks so a single copy of the VFS→layout conversion
// exists.
func ManifestInputFromVFS(src BlobSource, manifest *v1.Manifest, rawManifest []byte, platform *v1.Platform) ManifestInput {
	mi := ManifestInput{
		Manifest:     manifest,
		ManifestData: rawManifest,
		Config:       BlobFromSource(src, manifest.Config.Digest.Hex, manifest.Config.Size),
		Platform:     platform,
	}
	for _, layer := range manifest.Layers {
		mi.Layers = append(mi.Layers, LayerInput{
			Descriptor: layer,
			Blob:       BlobFromSource(src, layer.Digest.Hex, layer.Size),
			Present:    true,
		})
	}
	return mi
}

// RootInput describes one root of an IndexMultiRoot layout. A root is either a
// single image manifest or a multi-arch index; in both cases Children carries
// the concrete image manifests whose config and layer blobs must be written
// (for a manifest root, Children holds that single manifest).
type RootInput struct {
	// ManifestData is the exact raw bytes of the root object (an image manifest
	// or an image index). It is hashed for the digest and, for an index root,
	// written as a nested blob.
	ManifestData []byte
	// MediaType is the media type of the root object.
	MediaType types.MediaType
	// ArtifactType is set for non-image config media types; "" otherwise.
	ArtifactType string
	// IsIndex reports whether ManifestData is an image index (vs a manifest).
	IsIndex bool
	// OCITags are the full image names used for this root's index.json
	// annotations. Empty produces a single clean descriptor.
	OCITags []string
	// Children are the image manifests referenced by this root, in order. Their
	// manifest/config/layer blobs are written to the layout.
	Children []ManifestInput
	// Platform is carried for the first-single-arch docker-manifest selection.
	Platform *v1.Platform
}

// Builder accumulates manifests, tags and a Format, then writes a complete
// layout in one go.
type Builder struct {
	format Format

	repoTags []string // Docker manifest.json RepoTags
	ociTags  []string // index.json tag annotations

	progress     func(ctx context.Context, size int64, name string) io.Writer
	filter       ManifestFilter
	useSymlinks  bool
	requireLink  bool
	allowMissing bool
	missingHint  string // MissingBlobsError.OutputGroup

	manifests []ManifestInput
	roots     []RootInput // IndexMultiRoot roots (added via AddRoot)
	rootIndex *Blob
}

// New returns a Builder for the given Format.
func New(format Format) *Builder {
	return &Builder{format: format, missingHint: OutputGroupOCILayout}
}

// WithTags sets the Docker manifest.json RepoTags.
func (b *Builder) WithTags(repoTags []string) *Builder { b.repoTags = repoTags; return b }

// WithOCITags sets the tags used for index.json annotations.
func (b *Builder) WithOCITags(ociTags []string) *Builder { b.ociTags = ociTags; return b }

// WithAnnotationMode overrides the format's annotation mode (e.g. tag-only).
func (b *Builder) WithAnnotationMode(m AnnotationMode) *Builder {
	b.format = b.format.WithAnnotations(m)
	return b
}

// WithProgress sets a progress writer factory used while streaming blobs.
func (b *Builder) WithProgress(fn func(ctx context.Context, size int64, name string) io.Writer) *Builder {
	b.progress = fn
	return b
}

// WithManifestFilter sets a platform filter applied to index styles.
func (b *Builder) WithManifestFilter(f ManifestFilter) *Builder { b.filter = f; return b }

// WithLinkStrategy configures directory-sink blob materialization. useSymlinks
// symlinks instead of copying; requireLink errors instead of silently copying
// when a link is impossible.
func (b *Builder) WithLinkStrategy(useSymlinks, requireLink bool) *Builder {
	b.useSymlinks = useSymlinks
	b.requireLink = requireLink
	return b
}

// AllowMissingBlobs tolerates absent layer bodies instead of failing.
func (b *Builder) AllowMissingBlobs() *Builder { b.allowMissing = true; return b }

// WithMissingBlobsHint sets the output-group name reported in
// MissingBlobsError ("oci_layout" or "tarball").
func (b *Builder) WithMissingBlobsHint(outputGroup string) *Builder {
	b.missingHint = outputGroup
	return b
}

// AddManifest adds an image manifest to the layout.
func (b *Builder) AddManifest(m ManifestInput) *Builder {
	b.manifests = append(b.manifests, m)
	return b
}

// AddRoot adds one root to an IndexMultiRoot layout. Call it once per image or
// index to be referenced from the combined index.json.
func (b *Builder) AddRoot(r RootInput) *Builder {
	b.roots = append(b.roots, r)
	return b
}

// SetRootIndex supplies the root index bytes for the verbatim and wrapping
// index styles, and the root object for a sparse index layout.
func (b *Builder) SetRootIndex(index Blob) *Builder { b.rootIndex = &index; return b }

// WriteTo writes the layout to an arbitrary Sink.
func (b *Builder) WriteTo(ctx context.Context, sink Sink) error {
	return b.emit(ctx, sink)
}

// WriteDir writes the layout to a directory.
func (b *Builder) WriteDir(ctx context.Context, dir string) error {
	sink := NewDirectorySink(dir)
	if err := b.emit(ctx, sink); err != nil {
		return err
	}
	return sink.Close()
}

// WriteTar writes the layout as a tar to output ("-" for stdout). On error a
// partial output file is removed so a failed run leaves no output behind.
func (b *Builder) WriteTar(ctx context.Context, output string) error {
	sink, err := NewTarFileSink(output)
	if err != nil {
		return err
	}
	if err := b.emit(ctx, sink); err != nil {
		sink.Close()
		if output != "-" {
			os.Remove(output)
		}
		return err
	}
	return sink.Close()
}

// WriteToWriter writes the layout as a tar to an arbitrary io.Writer (used by
// the load pipeline to stream into a pipe).
func (b *Builder) WriteToWriter(ctx context.Context, w io.Writer) error {
	sink := NewTarSink(w)
	if err := b.emit(ctx, sink); err != nil {
		return err
	}
	return sink.Close()
}
