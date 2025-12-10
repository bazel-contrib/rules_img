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
    layers = "Layers of the image as list of LayerInfo.",
    missing_blobs = """List of hex-encoded sha256 hashes.
Used to convey information lost during shallow image pulling, where the base image layers are referenced, but never materialized.
""",
)

ImageManifestInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
