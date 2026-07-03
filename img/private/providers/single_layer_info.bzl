"""Defines providers for the image_layer rule."""

DOC = """\
Information about a single layer as a component of a container image.
"""

_metadata_doc = """\
File containing metadata about the layer blob as a JSON file with the following keys:
    - name: A human readable name for this layer. This includes the label of the layer or another descriptor (for anonymous layers, including those coming from pulled images).
    - diff_id: The diff ID of the layer as a string. Example: sha256:1234567890abcdef.
    - mediaType: The media type of the layer as a string. Example: application/vnd.oci.image.layer.v1.tar+gzip.
    - digest: The sha256 hash of the layer as a string. Example: sha256:1234567890abcdef.
    - size: The size of the layer in bytes as an int.
"""

FIELDS = dict(
    blob = "File containing the raw layer or None (for shallow base images or compact-stream-only mode).",
    metadata = _metadata_doc,
    media_type = "The media type of the layer as a string. Example: application/vnd.oci.image.layer.v1.tar+gzip.",
    estargz = "Boolean indicating whether the layer is an estargz layer.",
    compact_stream = "File containing the compact stream (.cstream) for the layer, or None.",
    layer_input_files = "Depset of files that went into this layer, or None.",
    layer_input_files_cas = "Tree artifact (directory) with the layer's input files content-addressed at sha256/<hex>, used to reconstruct the layer from its compact stream, or None.",
    sources = """List of upstream sources this layer's blob can be fetched from.

Each entry is a `struct(registry = <string>, repository = <string>)`. The blob is
content-addressed by its own digest, so only the registry/repository are recorded.
This is populated for layers that originate from a pulled base image (see
`image_import`) -- both shallow layers and eagerly downloaded ones -- so that at
deploy time a missing blob can be fetched from its original source registry. It is
an empty list for layers that are built locally and have no upstream origin.
""",
    mtree = "File containing an mtree(8) description of the layer's tar metadata, or None (for non-tar layers or shallow layers whose blob is unavailable).",
)

SingleLayerInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
