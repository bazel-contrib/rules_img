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

# The media type recorded for an ext4 filesystem image layer. It is not an OCI
# layer media type: such a layer is meant to be used on its own, not as part of
# an image, so it is not one of the LAYER_TYPES.
EXT4_LAYER = "application/x-ext4"

# The media type recorded for a FAT32 filesystem image layer. Like an ext4
# layer, it is not an OCI layer and not one of the LAYER_TYPES.
FAT32_LAYER = "application/x-fat32"

# The media types recorded for a cpio archive (an initramfs), by compression.
# Like an ext4 layer, a cpio layer is not an OCI layer and not a LAYER_TYPE.
CPIO_LAYER = "application/x-cpio"
CPIO_GZIP_LAYER = "application/x-cpio+gzip"
CPIO_ZSTD_LAYER = "application/x-cpio+zstd"

CPIO_LAYER_TYPES = [
    CPIO_LAYER,
    CPIO_GZIP_LAYER,
    CPIO_ZSTD_LAYER,
]

LAYER_TYPES = [
    UNCOMPRESSED_LAYER,
    GZIP_LAYER,
    ZSTD_LAYER,
] + EROFS_LAYER_TYPES

def media_type_is_ext4(media_type):
    """Whether a layer blob with this media type is an ext4 filesystem image.

    Args:
        media_type: Layer media type string, or None.

    Returns:
        True if the media type is the ext4 layer media type.
    """
    return media_type == EXT4_LAYER

def media_type_is_fat32(media_type):
    """Whether a layer blob with this media type is a FAT32 filesystem image.

    Args:
        media_type: Layer media type string, or None.

    Returns:
        True if the media type is the FAT32 layer media type.
    """
    return media_type == FAT32_LAYER

def media_type_is_cpio(media_type):
    """Whether a layer blob with this media type is a cpio archive.

    Args:
        media_type: Layer media type string, or None.

    Returns:
        True if the media type belongs to the cpio layer family.
    """
    return media_type != None and media_type in CPIO_LAYER_TYPES

def media_type_is_erofs(media_type):
    """Whether a layer blob with this media type is an EROFS filesystem image.

    Args:
        media_type: Layer media type string, or None.

    Returns:
        True if the media type belongs to the EROFS layer family.
    """
    return media_type != None and media_type in EROFS_LAYER_TYPES
