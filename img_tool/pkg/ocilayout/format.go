package ocilayout

import "fmt"

// MarkerKind selects the layout marker file.
type MarkerKind int

const (
	// MarkerOCILayout writes an "oci-layout" file.
	MarkerOCILayout MarkerKind = iota
	// MarkerSparse writes a "sparse-oci-layout" file.
	MarkerSparse
)

// IndexStyle selects how the root of the layout is expressed.
type IndexStyle int

const (
	// IndexClean writes index.json with a single manifest descriptor and no
	// tag annotations (the plain oci-layout single-manifest form).
	IndexClean IndexStyle = iota
	// IndexAnnotated writes index.json with one descriptor per tag carrying
	// containerd/apple/oci ref-name annotations (the docker-save form).
	IndexAnnotated
	// IndexVerbatim copies the supplied index bytes as index.json unmodified.
	IndexVerbatim
	// IndexWrapping writes a generated root index.json whose descriptor(s)
	// point at the supplied index stored as a nested blob (docker-save --index).
	IndexWrapping
	// IndexRootDescriptor writes root.descriptor.json instead of index.json
	// (sparse layout).
	IndexRootDescriptor
	// IndexMultiRoot writes a generated annotated index.json referencing every
	// root added via Builder.AddRoot: image-manifest roots are referenced
	// directly, index roots are stored as nested blobs and referenced by their
	// own index digest. Unlike the single-root styles it accepts N independent
	// roots, so it is used by the deploy oci-tar/docker-save sinks that combine
	// all operations into one layout.
	IndexMultiRoot
)

// BlobPolicy selects whether layer bodies are written.
type BlobPolicy int

const (
	// BlobIncludeAll writes every layer blob.
	BlobIncludeAll BlobPolicy = iota
	// BlobSparseLayers omits layer bodies and instead writes
	// <hex>.descriptor.json (and optional <hex>.cstream) files.
	BlobSparseLayers
)

// AnnotationMode selects how the org.opencontainers.image.ref.name annotation
// is populated for annotated index descriptors.
type AnnotationMode int

const (
	// AnnotateFullRef uses the full image reference (skopeo/rules_oci compatible).
	AnnotateFullRef AnnotationMode = iota
	// AnnotateTagOnly uses just the tag component (strict OCI spec form).
	AnnotateTagOnly
	// AnnotateNone adds no tag annotations (clean single-descriptor form).
	AnnotateNone
)

// Format captures the orthogonal choices that distinguish every layout the img
// tool produces. Presets cover the standard combinations; With* methods refine
// them. A Format value is immutable — With* returns a copy.
type Format struct {
	marker         MarkerKind
	indexStyle     IndexStyle
	dockerManifest bool
	blobPolicy     BlobPolicy
	annotation     AnnotationMode
}

// OCILayout is a plain OCI image layout: oci-layout marker, a clean index.json,
// no Docker manifest.json, all blobs included.
func OCILayout() Format {
	return Format{marker: MarkerOCILayout, indexStyle: IndexClean, blobPolicy: BlobIncludeAll, annotation: AnnotateNone}
}

// DockerSave is a Docker "save" tarball/directory: oci-layout marker, an
// annotated index.json, a Docker manifest.json, all blobs included.
func DockerSave() Format {
	return Format{marker: MarkerOCILayout, indexStyle: IndexAnnotated, dockerManifest: true, blobPolicy: BlobIncludeAll, annotation: AnnotateFullRef}
}

// OCILayoutFromIndex is an OCI image layout whose index.json is the supplied
// index copied verbatim.
func OCILayoutFromIndex() Format {
	return Format{marker: MarkerOCILayout, indexStyle: IndexVerbatim, blobPolicy: BlobIncludeAll, annotation: AnnotateNone}
}

// SparseOCILayout is the sparse layout: sparse-oci-layout marker, a
// root.descriptor.json, no Docker manifest.json, layer bodies replaced by
// descriptor stubs.
func SparseOCILayout() Format {
	return Format{marker: MarkerSparse, indexStyle: IndexRootDescriptor, blobPolicy: BlobSparseLayers, annotation: AnnotateNone}
}

// WithIndexStyle returns a copy with the given index style.
func (f Format) WithIndexStyle(s IndexStyle) Format { f.indexStyle = s; return f }

// WithDockerManifest returns a copy that emits (or not) a Docker manifest.json.
func (f Format) WithDockerManifest(on bool) Format { f.dockerManifest = on; return f }

// WithAnnotations returns a copy with the given annotation mode.
func (f Format) WithAnnotations(m AnnotationMode) Format { f.annotation = m; return f }

// WithMarker returns a copy with the given marker.
func (f Format) WithMarker(k MarkerKind) Format { f.marker = k; return f }

// WithBlobPolicy returns a copy with the given blob policy.
func (f Format) WithBlobPolicy(p BlobPolicy) Format { f.blobPolicy = p; return f }

// tagOnly reports whether ref.name annotations should be the tag only.
func (f Format) tagOnly() bool { return f.annotation == AnnotateTagOnly }

// needsRootIndex reports whether the format requires a root index blob to be
// supplied via Builder.SetRootIndex.
func (f Format) needsRootIndex() bool {
	return f.indexStyle == IndexVerbatim || f.indexStyle == IndexWrapping
}

// markerFile returns the marker filename and version string.
func (f Format) markerFile() (name, version string) {
	if f.marker == MarkerSparse {
		return "sparse-oci-layout", SparseOCILayoutVersion
	}
	return "oci-layout", OCILayoutVersion
}

// Validate rejects nonsensical combinations early and loudly.
func (f Format) Validate() error {
	if f.marker == MarkerSparse && f.dockerManifest {
		return fmt.Errorf("ocilayout: sparse layout cannot emit a Docker manifest.json")
	}
	if f.indexStyle == IndexRootDescriptor && f.dockerManifest {
		return fmt.Errorf("ocilayout: root.descriptor.json layout cannot emit a Docker manifest.json")
	}
	return nil
}
