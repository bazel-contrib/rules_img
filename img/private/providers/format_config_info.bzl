"""Providers describing the format-specific options of a filesystem-image layer."""

EXT4_DOC = """\
Options of an ext4 filesystem image layer, produced by the `ext4_config` rule
and selected per target through the `format_config` attribute of the layer
rules.
"""

EXT4_FIELDS = dict(
    block_size = "int: filesystem block size in bytes, or 0 for the default (4096).",
    min_size = "string: minimum image size with an optional binary unit, or \"\" for an image as large as its content needs.",
    bytes_per_inode = "string: reserve one inode per this many bytes of `min_size`, or \"\" for only the inodes the content needs.",
    extra_inodes = "int: free inodes to leave beyond those the content uses.",
    label = "string: volume label (at most 16 bytes), or \"\" for none.",
    uuid = "string: filesystem UUID, or \"\" to derive one from the content.",
    omit_lost_and_found = "bool: leave out the lost+found directory.",
    deduplicate = "string: \"auto\", \"enabled\" or \"disabled\"; whether identical files are stored once.",
)

Ext4ConfigInfo = provider(
    doc = EXT4_DOC,
    fields = EXT4_FIELDS,
)

EROFS_DOC = """\
Options of an EROFS filesystem image layer, produced by the `erofs_config` rule
and selected per target through the `format_config` attribute of the layer
rules.
"""

EROFS_FIELDS = dict(
    block_size = "int: filesystem block size in bytes, or 0 for the default (4096).",
    inline_threshold = "int: cap on tail-packing file payloads into inodes, or -1 for the global default.",
    synthetic_directories = "string: \"auto\", \"inherit\", \"default\" or \"strict\"; what to do about directories nothing describes.",
)

ErofsConfigInfo = provider(
    doc = EROFS_DOC,
    fields = EROFS_FIELDS,
)
