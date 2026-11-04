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

FAT32_DOC = """\
Options of a FAT32 filesystem image layer, produced by the `fat32_config` rule
and selected per target through the `format_config` attribute of the layer
rules.
"""

FAT32_FIELDS = dict(
    sector_size = "int: logical sector size in bytes (512, 1024, 2048 or 4096), or 0 for the default (512).",
)

Fat32ConfigInfo = provider(
    doc = FAT32_DOC,
    fields = FAT32_FIELDS,
)

ISO9660_DOC = """\
Options of an ISO 9660 filesystem image layer, produced by the `iso9660_config`
rule and selected per target through the `format_config` attribute of the layer
rules.
"""

ISO9660_FIELDS = dict(
    volume_id = "string: volume identifier (at most 32 characters), or \"\" for the default (\"CDROM\").",
    system_id = "string: system identifier (at most 32 characters), or \"\" for none.",
    volume_set_id = "string: volume set identifier (at most 128 characters), or \"\" for none.",
    publisher = "string: publisher identifier (at most 128 characters), or \"\" for none.",
    data_preparer = "string: data preparer identifier (at most 128 characters), or \"\" for none.",
    application_id = "string: application identifier (at most 128 characters), or \"\" for none.",
    rock_ridge = "string: \"auto\", \"enabled\", \"rationalized\" or \"disabled\"; whether the image carries the Rock Ridge extension, and whether its metadata is normalized.",
    joliet = "bool: whether the image carries a second, Joliet hierarchy with UCS-2 names.",
    joliet_long_names = "bool: whether Joliet names may be longer than the 64 characters of the specification.",
    udf = "bool: whether the image carries UDF structures describing the same file data.",
    deduplicate = "string: \"auto\", \"enabled\" or \"disabled\"; whether identical files share one extent.",
    min_size = "string: minimum image size with an optional binary unit, or \"\" for an image as large as its content needs.",
    boot = "list: the El Torito boot entries of the image, in order, as Iso9660BootEntryInfo providers.",
)

Iso9660ConfigInfo = provider(
    doc = ISO9660_DOC,
    fields = ISO9660_FIELDS,
)

ISO9660_BOOT_ENTRY_DOC = """\
One El Torito boot entry of an ISO 9660 image, produced by the
`iso9660_boot_entry` rule and passed to the `boot` attribute of
`iso9660_config`.
"""

ISO9660_BOOT_ENTRY_FIELDS = dict(
    image = "string: the path of the boot image in the layer, without a leading slash.",
    platform = "string: \"bios\", \"powerpc\", \"mac\" or \"uefi\".",
    emulation = "string: \"none\", \"floppy120\", \"floppy144\", \"floppy288\" or \"harddisk\".",
    load_size = "int: how many 512 byte sectors the firmware loads, or 0 for the whole image.",
    load_segment = "int: the real mode segment a BIOS entry is loaded at, or 0 for the default.",
    boot_info_table = "bool: whether the boot information table isolinux expects is patched into the image.",
    id = "string: the identifier recorded on the section header of an entry other than the first.",
)

Iso9660BootEntryInfo = provider(
    doc = ISO9660_BOOT_ENTRY_DOC,
    fields = ISO9660_BOOT_ENTRY_FIELDS,
)

CPIO_DOC = """\
Options of a cpio archive layer, produced by the `cpio_config` rule and selected
per target through the `format_config` attribute of the layer rules.
"""

CPIO_FIELDS = dict(
    deduplicate = "string: \"auto\", \"enabled\" or \"disabled\"; whether identical files are stored once.",
)

CpioConfigInfo = provider(
    doc = CPIO_DOC,
    fields = CPIO_FIELDS,
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
