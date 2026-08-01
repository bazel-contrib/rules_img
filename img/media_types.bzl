"""Well-defined media types for OCI container image layers.

This module provides constants for the standard OCI layer media types.
Use these constants when specifying layer compression formats.

See: https://specs.opencontainers.org/image-spec/layer/
"""

load(
    "//img/private/common:media_types.bzl",
    _EROFS_LAYER = "EROFS_LAYER",
    _EROFS_ZSTD_LAYER = "EROFS_ZSTD_LAYER",
    _GZIP_LAYER = "GZIP_LAYER",
    _UNCOMPRESSED_LAYER = "UNCOMPRESSED_LAYER",
    _ZSTD_LAYER = "ZSTD_LAYER",
)

UNCOMPRESSED_LAYER = _UNCOMPRESSED_LAYER
GZIP_LAYER = _GZIP_LAYER
ZSTD_LAYER = _ZSTD_LAYER

# EROFS layers: the blob is a mountable EROFS filesystem image rather than a tar
# changeset. See docs/erofs-layers.md.
EROFS_LAYER = _EROFS_LAYER
EROFS_ZSTD_LAYER = _EROFS_ZSTD_LAYER
