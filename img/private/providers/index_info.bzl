"""Defines providers for the image_index rule."""

DOC = """\
Information about a (multi-platform) image index (a collection of images).
"""

FIELDS = dict(
    descriptor = "File containing the descriptor of the index.",
    index = "File containing the raw image index (application/vnd.oci.image.index.v1+json).",
    manifests = "ImageManifestInfo of the images.",
    sparse_oci_layout = "Tree artifact containing the sparse OCI layout for this index.",
)

ImageIndexInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
