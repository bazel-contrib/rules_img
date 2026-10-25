"""Defines the provider for a disk image."""

DOC = """\
Describes a disk image with a partition table.

If the disk image is built as a compact stream, `image` is the real disk image
that can be reconstructed from it, a file that is only built when something
requests it.
"""

FIELDS = dict(
    image = "File with the raw disk image. In compact stream mode, it is only built on request.",
    compact_stream = "File with the compact stream of the disk image, or None if it was not requested.",
    layout = "JSON file that describes the layout of the disk: the place and the GUIDs of the partitions.",
    cas_dirs = "Depset of content-addressed directories (tree artifacts) with the blobs that `compact_stream` references.",
)

DiskImageInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
