"""Defines the provider for a partition of a disk image."""

DOC = """\
Describes one partition of a disk image: its place in the partition table
and the file system image that goes into it.
"""

FIELDS = dict(
    label = "Name of the partition (stored in a GPT).",
    type = "Partition type: a name like `esp` or `linux`, or a GUID.",
    guid = "Unique GUID of the partition, or an empty string to derive one.",
    size = "Size of the partition (like `64MiB`), or `auto` / an empty string to fit the contents.",
    attributes = "List of GPT attribute names.",
    mbr_type = "Type byte of the partition in an MBR, or 0 to derive it from the type.",
    active = "Whether the partition is marked bootable in an MBR.",
    hidden_sectors = "Whether the hidden sectors of a FAT32 file system are set to the start of the partition.",
    image = "File with the file system image, or None.",
    compact_stream = "File with the compact stream of the file system image, or None.",
    cas_dirs = "List of content-addressed directories (tree artifacts) with the blobs the compact stream references.",
)

PartitionInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
