"""Rules that configure the filesystem image a layer is written as."""

load("//img/private/providers:format_config_info.bzl", "CpioConfigInfo", "ErofsConfigInfo", "Ext4ConfigInfo")

def _cpio_config_impl(ctx):
    return [CpioConfigInfo(
        deduplicate = ctx.attr.deduplicate,
    )]

cpio_config = rule(
    implementation = _cpio_config_impl,
    doc = """\
Options for layers written as a cpio archive.

Pass the target to the `format_config` attribute of a layer rule that uses
`format = "cpio"`. Options that are left at their default are decided by the
`img` tool or by the matching global setting. How the archive is compressed is
not an option of the format: it is the `compress` attribute of the layer.

Example:

```python
load("@rules_img//img:layer.bzl", "cpio_config", "image_layer")

cpio_config(
    name = "initramfs_config",
    # Store every file, even if two have the same content.
    deduplicate = "disabled",
)

image_layer(
    name = "initramfs",
    srcs = {"/init": ":init"},
    compress = "zstd",
    format = "cpio",
    format_config = ":initramfs_config",
)
```

See [docs/filesystems.md](filesystems.md) for the format itself.
""",
    attrs = {
        "deduplicate": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = """\
Whether regular files with the same content and metadata are stored once, as hard
links of each other (what tar layers do). If set to 'auto', uses the global default
`--@rules_img//img/settings:cpio_deduplicate` setting.

Turning it off spares reading file content only to find duplicates, at the cost of
a larger archive.
""",
        ),
    },
    provides = [CpioConfigInfo],
)

def _ext4_config_impl(ctx):
    if ctx.attr.block_size != 0 and (ctx.attr.block_size < 1024 or ctx.attr.block_size > 65536 or ctx.attr.block_size & (ctx.attr.block_size - 1) != 0):
        fail("block_size must be a power of two between 1024 and 65536, got {}".format(ctx.attr.block_size))
    if len(ctx.attr.label) > 16:
        fail("label {} is longer than 16 bytes".format(repr(ctx.attr.label)))
    if ctx.attr.extra_inodes < 0:
        fail("extra_inodes must not be negative, got {}".format(ctx.attr.extra_inodes))
    return [Ext4ConfigInfo(
        block_size = ctx.attr.block_size,
        min_size = ctx.attr.min_size,
        bytes_per_inode = ctx.attr.bytes_per_inode,
        extra_inodes = ctx.attr.extra_inodes,
        label = ctx.attr.label,
        uuid = ctx.attr.uuid,
        omit_lost_and_found = ctx.attr.omit_lost_and_found,
        deduplicate = ctx.attr.deduplicate,
    )]

ext4_config = rule(
    implementation = _ext4_config_impl,
    doc = """\
Options for layers written as an ext4 filesystem image.

Pass the target to the `format_config` attribute of a layer rule that uses
`format = "ext4"`. Options that are left at their default are decided by the
`img` tool (or, for `deduplicate`, by the global
`--@rules_img//img/settings:ext4_deduplicate` setting).

Example:

```python
load("@rules_img//img:layer.bzl", "ext4_config", "image_layer")

ext4_config(
    name = "rootfs_config",
    # Mountable on a machine with 4 KiB pages or more.
    block_size = 4096,
    label = "rootfs",
    # Room to write into the filesystem once it is mounted.
    min_size = "512MiB",
    bytes_per_inode = "16KiB",
)

image_layer(
    name = "rootfs",
    srcs = {"/etc/hostname": ":hostname"},
    format = "ext4",
    format_config = ":rootfs_config",
)
```

See [docs/filesystems.md](filesystems.md) for the format itself.
""",
    attrs = {
        "block_size": attr.int(
            default = 0,
            doc = """\
Filesystem block size in bytes: a power of two between 1024 and 65536. 0 uses 4096.

Linux can only mount a filesystem whose block size is at most the page size of the
machine, so 4096 is the portable choice and 65536 only mounts on a machine with
64 KiB pages.
""",
        ),
        "min_size": attr.string(
            default = "",
            doc = """\
Minimum size of the image, with an optional binary unit (`"512MiB"`, `"2GiB"`).

The filesystem is otherwise exactly as large as its content needs. A larger size
leaves the extra space free, which is what a filesystem that is going to be written
to needs. Blocks that hold nothing are not written, so a sparse-aware filesystem
stores such an image in about the size of its content.
""",
        ),
        "bytes_per_inode": attr.string(
            default = "",
            doc = """\
Reserve one inode for every this many bytes of `min_size`, in addition to the inodes the
content needs, with an optional binary unit (`"16KiB"`).

Only useful together with `min_size`: it decides how many files can be created in the
free space. Empty leaves only the inodes the content needs, plus `extra_inodes`.
""",
        ),
        "extra_inodes": attr.int(
            default = 0,
            doc = "Number of free inodes to leave beyond those the content uses.",
        ),
        "label": attr.string(
            default = "",
            doc = "Volume label of the filesystem, at most 16 bytes.",
        ),
        "uuid": attr.string(
            default = "",
            doc = """\
Filesystem UUID, in the usual textual form (`"01234567-89ab-cdef-0123-456789abcdef"`).

Empty derives a UUID from the metadata of the content, so that equal inputs produce
equal images. Set it when something outside the image refers to the filesystem by
UUID, for example an `/etc/fstab` entry or a bootloader configuration.
""",
        ),
        "omit_lost_and_found": attr.bool(
            default = False,
            doc = """\
Leave out the `lost+found` directory, which fsck uses to reattach orphaned files.

An image that is only read does not need it, and leaving it out makes the layer's
`mtree` match the one a tar or EROFS layer of the same content produces. `fsck`
reports the directory as missing.
""",
        ),
        "deduplicate": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = """\
Whether regular files with the same content and metadata are stored once, as hard
links of each other (what tar layers do). If set to 'auto', uses the global default
`--@rules_img//img/settings:ext4_deduplicate` setting.

Turning it off spares reading file content only to find duplicates, at the cost of
a larger image.
""",
        ),
    },
    provides = [Ext4ConfigInfo],
)

def _erofs_config_impl(ctx):
    if ctx.attr.block_size != 0 and (ctx.attr.block_size < 512 or ctx.attr.block_size & (ctx.attr.block_size - 1) != 0):
        fail("block_size must be a power of two of at least 512, got {}".format(ctx.attr.block_size))
    if ctx.attr.inline_threshold < -1:
        fail("inline_threshold must not be negative, got {}".format(ctx.attr.inline_threshold))
    return [ErofsConfigInfo(
        block_size = ctx.attr.block_size,
        inline_threshold = ctx.attr.inline_threshold,
        synthetic_directories = ctx.attr.synthetic_directories,
    )]

erofs_config = rule(
    implementation = _erofs_config_impl,
    doc = """\
Options for layers written as an EROFS filesystem image.

Pass the target to the `format_config` attribute of a layer rule that uses
`format = "erofs"`. Options that are left at their default are decided by the
`img` tool or by the matching global setting.

Example:

```python
load("@rules_img//img:layer.bzl", "erofs_config", "image_layer")

erofs_config(
    name = "layer_config",
    block_size = 4096,
)

image_layer(
    name = "layer",
    srcs = {"/etc/hostname": ":hostname"},
    format = "erofs",
    format_config = ":layer_config",
)
```

See [docs/erofs-layers.md](erofs-layers.md) for the format itself.
""",
    attrs = {
        "block_size": attr.int(
            default = 0,
            doc = """\
Filesystem block size in bytes: a power of two of at least 512. 0 uses 4096, which
matches the common page size.
""",
        ),
        "inline_threshold": attr.int(
            default = -1,
            doc = """\
Maximum size (in bytes) of a file payload the layer may tail-pack into its inode.

A file at least this large gets a data extent of its own, and therefore becomes a
content-addressed reference in the compact stream rather than inline bytes. 0 packs
whatever fits in the inode's block. -1 uses the global default
`--@rules_img//img/settings:erofs_inline_threshold` setting.
""",
        ),
        "synthetic_directories": attr.string(
            default = "auto",
            values = ["auto", "inherit", "default", "strict"],
            doc = """\
Attributes for directories the layer has to invent because nothing describes them.

'inherit' copies the nearest described ancestor's ownership (what mkfs.erofs does),
'default' uses 0755 root:root, 'strict' fails the layer instead of guessing. If set
to 'auto', uses the global default
`--@rules_img//img/settings:erofs_synthetic_directories` setting.
""",
        ),
    },
    provides = [ErofsConfigInfo],
)
