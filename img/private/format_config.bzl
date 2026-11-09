"""Rules that configure the filesystem image a layer is written as."""

load("//img/private/providers:format_config_info.bzl", "CpioConfigInfo", "ErofsConfigInfo", "Ext4ConfigInfo", "Fat32ConfigInfo", "Iso9660BootEntryInfo", "Iso9660ConfigInfo")

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

def _fat32_config_impl(ctx):
    if ctx.attr.sector_size not in [0, 512, 1024, 2048, 4096]:
        fail("sector_size must be 512, 1024, 2048 or 4096, got {}".format(ctx.attr.sector_size))
    return [Fat32ConfigInfo(
        sector_size = ctx.attr.sector_size,
    )]

fat32_config = rule(
    implementation = _fat32_config_impl,
    doc = """\
Options for layers written as a FAT32 filesystem image.

Pass the target to the `format_config` attribute of a layer rule that uses
`format = "fat32"`. Options that are left at their default are decided by the
`img` tool.

Example:

```python
load("@rules_img//img:layer.bzl", "fat32_config", "image_layer")

fat32_config(
    name = "esp_config",
    # The sector size of the disk the file system ends up in.
    sector_size = 4096,
)

image_layer(
    name = "esp",
    srcs = {"/EFI/BOOT/BOOTAA64.EFI": ":bootloader"},
    format = "fat32",
    format_config = ":esp_config",
)
```

See [docs/filesystems.md](filesystems.md) for the format itself.
""",
    attrs = {
        "sector_size": attr.int(
            default = 0,
            doc = """\
Logical sector size of the file system in bytes: 512, 1024, 2048 or 4096. 0 uses 512.

A FAT32 file system in a partition of a disk with 4096 byte sectors has to use the
sector size of the disk. A larger sector size makes the smallest possible volume
larger, because FAT32 needs at least 65525 clusters.
""",
        ),
    },
    provides = [Fat32ConfigInfo],
)

_BOOT_PLATFORMS = ["bios", "powerpc", "mac", "uefi"]
_BOOT_EMULATIONS = ["none", "floppy120", "floppy144", "floppy288", "harddisk"]

def _iso9660_boot_entry_impl(ctx):
    if ctx.attr.load_size < 0 or ctx.attr.load_size > 0xFFFF:
        fail("load_size must be between 0 and 65535, got {}".format(ctx.attr.load_size))
    if ctx.attr.load_segment < 0 or ctx.attr.load_segment > 0xFFFF:
        fail("load_segment must be between 0 and 65535, got {}".format(ctx.attr.load_segment))
    if len(ctx.attr.id) > 28:
        fail("id {} is longer than 28 characters".format(repr(ctx.attr.id)))
    return [Iso9660BootEntryInfo(
        image = ctx.attr.image.removeprefix("/"),
        platform = ctx.attr.platform,
        emulation = ctx.attr.emulation,
        load_size = ctx.attr.load_size,
        load_segment = ctx.attr.load_segment,
        boot_info_table = ctx.attr.boot_info_table,
        id = ctx.attr.id,
    )]

iso9660_boot_entry = rule(
    implementation = _iso9660_boot_entry_impl,
    doc = """\
One El Torito boot entry of an ISO 9660 image.

El Torito makes an image bootable by pointing firmware at a boot image that is
an ordinary file of the filesystem. Pass the targets to the `boot` attribute of
an `iso9660_config`, in the order the boot catalog should list them: the first
one becomes the default entry, and every further one becomes a section of its
own, so a single image can boot on BIOS and on UEFI.

Example:

```python
load("@rules_img//img:layer.bzl", "image_layer", "iso9660_boot_entry", "iso9660_config")

# A BIOS boot sector that wants the boot information table patched in.
iso9660_boot_entry(
    name = "bios_boot",
    boot_info_table = True,
    image = "/isolinux/isolinux.bin",
    load_size = 4,
    platform = "bios",
)

# A FAT image the firmware mounts and reads the boot loader from.
iso9660_boot_entry(
    name = "efi_boot",
    id = "EFI",
    image = "/EFI/BOOT/efiboot.img",
    platform = "uefi",
)

iso9660_config(
    name = "installer_config",
    boot = [
        ":bios_boot",
        ":efi_boot",
    ],
    joliet = True,
)

image_layer(
    name = "installer",
    srcs = {
        "/isolinux/isolinux.bin": ":isolinux",
        "/EFI/BOOT/efiboot.img": ":esp_image",
    },
    format = "iso9660",
    format_config = ":installer_config",
)
```

See [docs/filesystems.md](filesystems.md) for the format itself.
""",
    attrs = {
        "image": attr.string(
            mandatory = True,
            doc = """\
Path of the boot image in the layer, which has to be a regular file with content.

A leading slash is optional. The file is stored once: the boot entry points at
the extent the directory record points at.
""",
        ),
        "platform": attr.string(
            default = "bios",
            values = _BOOT_PLATFORMS,
            doc = """\
The firmware the entry is for.

'uefi' is what a current PC boots from; its boot image is usually a FAT
filesystem (an `image_layer` with `format = "fat32"`) that the firmware mounts
and reads the boot loader from.
""",
        ),
        "emulation": attr.string(
            default = "none",
            values = _BOOT_EMULATIONS,
            doc = """\
What the firmware emulates while it reads the boot image.

'none' loads the image as it is, which is what every current boot loader and
every UEFI firmware wants. The floppy and hard disk modes present the image as a
drive of that shape.
""",
        ),
        "load_size": attr.int(
            default = 0,
            doc = """\
How many 512 byte sectors the firmware loads.

0 loads the whole image (clamped to the 65535 sectors the field can hold), which
is what a UEFI boot image needs. isolinux wants 4.
""",
        ),
        "load_segment": attr.int(
            default = 0,
            doc = "The real mode segment a BIOS entry is loaded at. 0 uses the default of 0x7C0.",
        ),
        "boot_info_table": attr.bool(
            default = False,
            doc = """\
Patch the boot information table isolinux and its relatives expect into bytes 8
to 64 of the boot image.

The stored content then differs from the file that went in, so the boot image is
always carried in the image itself and never becomes a reference in a compact
stream. A boot image that shares its content with another entry cannot be
patched.
""",
        ),
        "id": attr.string(
            default = "",
            doc = """\
Identifier recorded on the section header of an entry other than the first, at
most 28 characters.

"EFI" is conventional for a UEFI section, but nothing reads it.
""",
        ),
    },
    provides = [Iso9660BootEntryInfo],
)

def _iso9660_config_impl(ctx):
    for name, value, limit in [
        ("volume_id", ctx.attr.volume_id, 32),
        ("system_id", ctx.attr.system_id, 32),
        ("volume_set_id", ctx.attr.volume_set_id, 128),
        ("publisher", ctx.attr.publisher, 128),
        ("data_preparer", ctx.attr.data_preparer, 128),
        ("application_id", ctx.attr.application_id, 128),
    ]:
        if len(value) > limit:
            fail("{} {} is longer than {} characters".format(name, repr(value), limit))
    if ctx.attr.joliet_long_names and not ctx.attr.joliet:
        fail("joliet_long_names has no effect without joliet = True")
    return [Iso9660ConfigInfo(
        volume_id = ctx.attr.volume_id,
        system_id = ctx.attr.system_id,
        volume_set_id = ctx.attr.volume_set_id,
        publisher = ctx.attr.publisher,
        data_preparer = ctx.attr.data_preparer,
        application_id = ctx.attr.application_id,
        rock_ridge = ctx.attr.rock_ridge,
        joliet = ctx.attr.joliet,
        joliet_long_names = ctx.attr.joliet_long_names,
        udf = ctx.attr.udf,
        deduplicate = ctx.attr.deduplicate,
        min_size = ctx.attr.min_size,
        boot = [target[Iso9660BootEntryInfo] for target in ctx.attr.boot],
    )]

iso9660_config = rule(
    implementation = _iso9660_config_impl,
    doc = """\
Options for layers written as an ISO 9660 filesystem image.

Pass the target to the `format_config` attribute of a layer rule that uses
`format = "iso9660"`. Options that are left at their default are decided by the
`img` tool or by the matching global setting.

Plain ISO 9660 can only store upper case, 8.3-ish names and no POSIX metadata at
all, so the interesting part of the format is its extensions. Rock Ridge is on by
default; Joliet and UDF are not, because enabling them is a decision about who
reads the image.

Example:

```python
load("@rules_img//img:layer.bzl", "image_layer", "iso9660_config")

iso9660_config(
    name = "release_config",
    # Readable on Linux (Rock Ridge, on by default), on Windows (Joliet) and on
    # macOS (UDF).
    joliet = True,
    udf = True,
    volume_id = "MYPRODUCT_1_0",
)

image_layer(
    name = "release",
    srcs = {"/README.md": ":readme"},
    format = "iso9660",
    format_config = ":release_config",
)
```

See [docs/filesystems.md](filesystems.md) for the format itself and
`iso9660_boot_entry` for making the image bootable.
""",
    attrs = {
        "volume_id": attr.string(
            default = "",
            doc = """\
Volume identifier, at most 32 characters. Empty uses "CDROM".

It is what a mounted image is usually named after.
""",
        ),
        "system_id": attr.string(
            default = "",
            doc = "System identifier, at most 32 characters, naming the system that can act on the image's system area.",
        ),
        "volume_set_id": attr.string(
            default = "",
            doc = "Volume set identifier, at most 128 characters.",
        ),
        "publisher": attr.string(
            default = "",
            doc = "Publisher identifier, at most 128 characters.",
        ),
        "data_preparer": attr.string(
            default = "",
            doc = "Data preparer identifier, at most 128 characters.",
        ),
        "application_id": attr.string(
            default = "",
            doc = "Application identifier, at most 128 characters.",
        ),
        "rock_ridge": attr.string(
            default = "auto",
            values = ["auto", "enabled", "rationalized", "disabled"],
            doc = """\
Whether the image carries the Rock Ridge extension, which adds the real names,
the mode, the owner, the timestamps, symbolic links, device nodes, FIFOs and hard
links. This is what Linux uses.

'rationalized' additionally normalizes the metadata of every entry the way
`mkisofs -rational-rock` does: the owner becomes root, the mode becomes read-only
for everyone, the execute bit is kept and shared by all three classes, and the
setuid, setgid and sticky bits are cleared. It makes the image independent of
whoever built it, and it applies to the UDF metadata too, because that describes
the same entries.

Without the extension, symbolic links, device nodes and FIFOs cannot be stored and
a layer with such an entry fails. If set to 'auto', uses the global default
`--@rules_img//img/settings:iso9660_rock_ridge` setting.
""",
        ),
        "joliet": attr.bool(
            default = False,
            doc = """\
Add a second directory hierarchy with UCS-2 names, which is what Windows reads.

It describes the same file data, so it only costs its directory extents and path
tables. Names are capped at the 64 characters of the specification unless
`joliet_long_names` is set.
""",
        ),
        "joliet_long_names": attr.bool(
            default = False,
            doc = """\
Raise the Joliet name limit from the 64 characters of the specification to the
103 every implementation accepts.

Only useful together with `joliet`.
""",
        ),
        "udf": attr.bool(
            default = False,
            doc = """\
Add UDF 1.02 structures describing the same file data, in the layout known as a
UDF bridge image. This is what macOS and Windows prefer.

UDF has the real names and POSIX metadata without an extension and no 4 GiB file
size limit. It costs one 2 KiB block per file and directory for its File Entry,
plus the blocks of the directories' file identifier descriptors.
""",
        ),
        "deduplicate": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = """\
Whether regular files with the same content and metadata are stored once, sharing
one extent. With Rock Ridge they become hard links of each other (what tar layers
do). If set to 'auto', uses the global default
`--@rules_img//img/settings:iso9660_deduplicate` setting.

Turning it off spares reading file content only to find duplicates, at the cost of
a larger image.
""",
        ),
        "min_size": attr.string(
            default = "",
            doc = """\
Minimum size of the image, with an optional binary unit (`"64MiB"`, `"2GiB"`).

The image is otherwise exactly as large as its content needs. Blocks that hold
nothing are not written, so a sparse-aware filesystem stores such an image in
about the size of its content.
""",
        ),
        "boot": attr.label_list(
            providers = [Iso9660BootEntryInfo],
            doc = """\
El Torito boot entries, as `iso9660_boot_entry` targets, in the order the boot
catalog should list them.

The first one becomes the default entry; every further one becomes a section of
its own, so a single image can boot on BIOS and on UEFI.
""",
        ),
    },
    provides = [Iso9660ConfigInfo],
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
