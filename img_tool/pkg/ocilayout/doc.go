// Package ocilayout is the single writer for every container image layout
// format produced by the img tool: OCI image layouts (directory or tar),
// Docker "save" tarballs (the hybrid oci-layout + manifest.json form), and the
// sparse OCI layout (layer blobs replaced by descriptor stubs).
//
// It exposes two entry points that share one emission engine:
//
//   - Builder: create a complete layout in one go (WriteDir/WriteTar/
//     WriteToWriter). This is what the docker-save, oci-layout and
//     sparse-oci-layout subcommands and the load pipeline use.
//
//   - Editor: mutate an OCI layout directory in place — add blobs
//     (idempotent: a blob that already exists is a no-op) and union manifests
//     into an existing index.json.
//
// A layout is described by a Format value (marker file, index.json style,
// whether a Docker manifest.json is emitted, blob-inclusion policy and tag
// annotation mode). Preset constructors — OCILayout, DockerSave,
// OCILayoutFromIndex and SparseOCILayout — cover the standard combinations and
// can be refined with the With* methods.
//
// Blobs are described source-agnostically by a Blob value (in-memory bytes, a
// local file path, or a streaming opener). A directory sink hardlinks/reflinks
// file-path blobs; a tar sink streams them. The existing BlobSource streaming
// interface (used by the load pipeline's VFS) is kept verbatim and adapted via
// BlobFromSource.
package ocilayout

const (
	// OCILayoutVersion is written into the "oci-layout" marker file.
	OCILayoutVersion = "1.0.0"
	// SparseOCILayoutVersion is written into the "sparse-oci-layout" marker file.
	SparseOCILayoutVersion = "1.0.0"

	// mediaTypeOCIImageIndex is the media type of an OCI image index.
	mediaTypeOCIImageIndex = "application/vnd.oci.image.index.v1+json"
)
