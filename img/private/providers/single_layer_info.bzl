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
    blob = "File containing the raw layer or None (for shallow base images).",
    metadata = _metadata_doc,
    media_type = "The media type of the layer as a string. Example: application/vnd.oci.image.layer.v1.tar+gzip.",
    estargz = "Boolean indicating whether the layer is an estargz layer.",
)

SingleLayerInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
