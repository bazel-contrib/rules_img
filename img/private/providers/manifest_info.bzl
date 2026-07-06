"""Defines providers for the image_manifest rule."""

DOC = """\
Information about a single-platform container image (manifest, config, and layers).
"""

FIELDS = dict(
    descriptor = "File containing the descriptor of the manifest.",
    manifest = "File containing the raw image manifest (application/vnd.oci.image.index.v1+json).",
    config = "File containing the raw image config (application/vnd.oci.image.config.v1+json).",
    structured_config = "(Partial) image config with values known in the analysis phase.",
    architecture = "The CPU architecture this image runs on.",
    os = "The operating system this image runs on.",
    variant = "The platform variant (e.g., 'v3' for amd64/v3, 'v8' for arm64/v8).",
    layers = "Layers of the image as list of SingleLayerInfo.",
    mtree = "File with the image's mtree (the per-layer mtrees merged in layer order as an OCI applied changeset), or None when no layer contributes one.",
    sparse_oci_layout = "Tree artifact containing the sparse OCI layout for this manifest.",
)

ImageManifestInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
