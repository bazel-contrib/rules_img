"""
A list of well-defined media types for container image layers.
Source: https://specs.opencontainers.org/image-spec/layer/
"""

UNCOMPRESSED_LAYER = "application/vnd.oci.image.layer.v1.tar"
GZIP_LAYER = "application/vnd.oci.image.layer.v1.tar+gzip"
ZSTD_LAYER = "application/vnd.oci.image.layer.v1.tar+zstd"

# EROFS layers. The blob is a mountable EROFS filesystem image rather than a tar
# changeset; see docs/erofs-layers.md.
EROFS_LAYER = "application/vnd.erofs.layer.v1"
EROFS_ZSTD_LAYER = "application/vnd.erofs.layer.v1+zstd"

EROFS_LAYER_TYPES = [
    EROFS_LAYER,
    EROFS_ZSTD_LAYER,
]

# The media types recorded for ext4 and FAT32 filesystem image layers and for
# cpio archives (initramfs). They are not OCI layer media types: such a layer is meant to be used on its own, not as
# part of an image, so they are not among the LAYER_TYPES.
EXT4_LAYER = "application/x-ext4"
FAT32_LAYER = "application/x-fat32"
CPIO_LAYER = "application/x-cpio"
CPIO_GZIP_LAYER = "application/x-cpio+gzip"
CPIO_ZSTD_LAYER = "application/x-cpio+zstd"

LAYER_TYPES = [
    UNCOMPRESSED_LAYER,
    GZIP_LAYER,
    ZSTD_LAYER,
] + EROFS_LAYER_TYPES

def media_type_is_erofs(media_type):
    """Whether a layer blob with this media type is an EROFS filesystem image.

    Args:
        media_type: Layer media type string, or None.

    Returns:
        True if the media type belongs to the EROFS layer family.
    """
    return media_type != None and media_type in EROFS_LAYER_TYPES
