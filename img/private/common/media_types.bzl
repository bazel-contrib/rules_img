"""
A list of well-defined media types for container image layers.
Source: https://specs.opencontainers.org/image-spec/layer/
"""

UNCOMPRESSED_LAYER = "application/vnd.oci.image.layer.v1.tar"
GZIP_LAYER = "application/vnd.oci.image.layer.v1.tar+gzip"
ZSTD_LAYER = "application/vnd.oci.image.layer.v1.tar+zstd"

LAYER_TYPES = [
    UNCOMPRESSED_LAYER,
    GZIP_LAYER,
    ZSTD_LAYER,
]
