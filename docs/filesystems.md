# Filesystem formats

rules_img supports multiple filesystems. Every layer rule (`image_layer`,
`layer_from_binary`, `layer_from_tar`, `base_image_layer`, `oras_layer`)
takes a `format` attribute, and the global default comes from
`--@rules_img//img/settings:layer_format`:

| `format` | Blob                                      | A valid OCI layer?                | Extension                        |
|----------|-------------------------------------------|-----------------------------------|----------------------------------|
| `tar`    | OCI tar changeset                         | yes                               | `.tar`, `.tgz`, `.tar.zst`       |
| `erofs`  | [EROFS](erofs-layers.md) filesystem image | proposed, supported by containerd | `.erofs`, `.erofs.zst`           |
| `ext4`   | ext4 filesystem image                     | **no**                            | `.ext4`                          |
| `fat32`  | FAT32 filesystem image                    | **no**                            | `.fat32`                         |
| `iso9660`| ISO 9660 image                            | **no**                            | `.iso`                           |
| `cpio`   | cpio archive (initramfs)                  | **no**                            | `.cpio`, `.cpio.gz`, `.cpio.zst` |

`tar` is the default, and it is the only format currently specified in the
[OCI image spec](https://github.com/opencontainers/image-spec/blob/main/layer.md).
Every registry and runtime understands it, so unless you have a specific reason
to pick something else, pick tar. [EROFS](erofs-layers.md) is emerging as an
alternative on the wire. Containerd can mount such a layer directly instead of
unpacking it. `ext4`, `fat32`, `iso9660` and `cpio` are not valid OCI layer formats:
nothing in the ecosystem expects an ext4, FAT32 or ISO 9660 blob or a cpio archive in an image manifest.
Support for them in rules_img exists to build filesystem images and initramfs archives for other purposes.

## ext4

[ext4](https://docs.kernel.org/filesystems/ext4/) images are written by a pure-Go
implementation in the `img` tool, so building one doesn't require `mkfs.ext4`,
doesn't need root privileges, doesn't require a loopback device,
and works on every platform rules_img supports (including Windows).

```python
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(
    name = "rootfs",
    srcs = {
        "/etc/hostname": ":hostname",
        "/usr/bin/server": "//cmd/server",
    },
    format = "ext4",
)
```

The ext4 filesystem image is the layer target's `DefaultInfo` (or, with compact layers enabled,
its compact stream). Everything that is not tar-specific works as usual,
including compact layers (`experimental_compact_layers`) and the `mtree` output
group.

The image is only as large as its content needs.
Regular files with the same content are automatically deduplicated using hard links
of each other, just like in a tar layer:

```
# ext4 deduplication is enabled by default.
# This setting disables deduplication and stores each input as a regular file.
common --@rules_img//img/settings:ext4_deduplicate=disabled
```

The images are reproducible: the UUID and hash seed are derived from the
metadata of the content, timestamps default to the Unix epoch, and the layout is
a pure function of the inputs.

## Configuring the image

The geometry of the filesystem comes from an `ext4_config` target, passed to the
layer's `format_config` attribute. Pick a `min_size` larger than the content when
the image is going to be written to after it is mounted:

```python
load("@rules_img//img:layer.bzl", "ext4_config", "image_layer")

ext4_config(
    name = "rootfs_config",
    block_size = 4096,
    bytes_per_inode = "16KiB",
    label = "rootfs",
    min_size = "512MiB",
    uuid = "01234567-89ab-cdef-0123-456789abcdef",
)

image_layer(
    name = "rootfs",
    srcs = {"/etc/hostname": ":hostname"},
    format = "ext4",
    format_config = ":rootfs_config",
)
```

A few of them are worth knowing about:

| Option                | What it decides                                                                                                       |
|-----------------------|-----------------------------------------------------------------------------------------------------------------------|
| `block_size`          | Linux only mounts a filesystem whose block size is at most the page size, so 4096 (the default) is the portable choice |
| `min_size`            | free space to leave for whatever writes to the filesystem later; blocks that hold nothing are never written            |
| `bytes_per_inode`     | how many files may be created in that free space                                                                      |
| `uuid`                | set it when something refers to the filesystem by UUID (`/etc/fstab`, a bootloader); otherwise it is derived           |
| `omit_lost_and_found` | leaves out the `lost+found` directory, which makes the layer's `mtree` match a tar or EROFS layer of the same content  |

Some ext4 features are not yet supported:

| Feature                         | Status                                                                               |
|---------------------------------|--------------------------------------------------------------------------------------|
| journal                         | not written; the image is a build artifact, not a live filesystem                    |
| hashed (htree) directories      | directories are linear, which the kernel reads fine                                  |
| whiteouts (`.wh.`)              | not translated: an ext4 image is a filesystem, so a `.wh.` entry is an ordinary file |
| 64-bit block numbers            | images are limited to 2<sup>32</sup> blocks                                          |
| per-file compression, dm-verity | not supported                                                                        |

## fat32

A FAT32 layer is a FAT32 filesystem image, the filesystem of an EFI system partition (ESP) and of removable
media. Like the other formats, it is written by a pure-Go implementation in the `img` tool, so building one
doesn't require `mkfs.fat`, `mtools` or root privileges and works on every platform rules_img supports
(including Windows).

```python
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(
    name = "esp",
    srcs = {
        "/EFI/BOOT/BOOTAA64.EFI": ":bootloader",
        "/loader/loader.conf": ":loader_conf",
    },
    format = "fat32",
)
```

The image is the layer target's `DefaultInfo` (or, with compact layers enabled, its compact stream). Everything
that is not tar-specific works as usual, including compact layers (`experimental_compact_layers`) and the
`mtree` output group. The options that only apply to tar layers, including `compress`, are ignored. The
file extension is `.fat32`.

The file system is laid out completely before the first byte is written. The root directory and all other
directories come first, followed by the data of every regular file in the order the files were added, each in
a contiguous run of clusters that starts at a multiple of the cluster size. A file can therefore be located
from its offset and size alone, which is what makes compact streams of FAT32 images efficient: every file
of at least the inline threshold becomes a reference to the content-addressed store. All-zero sectors are never
written, and file contents are not held in memory while the image is built: they are read from where they
are when the image is written.

FAT32 volumes need at least 65525 clusters to be recognized as FAT32, so the smallest image is about
33 MiB with the smallest clusters. Unless a larger size is asked for, the file system has no more
clusters than that or than the content needs. Names are stored as long file names (UTF-16) with generated
short names. The volume ID is derived from the metadata of the content, and timestamps default to the
Unix epoch (clamped to 1 January 1980), so the layout is a pure function of the inputs.

The sector size comes from a `fat32_config` target, passed to the layer's `format_config` attribute. A FAT32
file system in a partition of a disk with 4096 byte sectors has to use the sector size of the disk:

```python
load("@rules_img//img:layer.bzl", "fat32_config", "image_layer")

fat32_config(
    name = "esp_config",
    sector_size = 4096,
)

image_layer(
    name = "esp",
    srcs = {"/EFI/BOOT/BOOTAA64.EFI": ":bootloader"},
    format = "fat32",
    format_config = ":esp_config",
)
```

FAT cannot store everything a tar layer can:

| Feature                       | Status                                                                                            |
|-------------------------------|---------------------------------------------------------------------------------------------------|
| symbolic links                | an error: FAT has none                                                                            |
| device nodes, FIFOs           | an error                                                                                          |
| hard links                    | become copies, and identical files are never merged                                               |
| ownership, permissions, xattrs| dropped, except for the read-only flag, which is set for files without the owner write bit         |
| timestamps                    | 2 second resolution between 1980 and 2107, stored as UTC (values outside are clamped); the access time only has a date |
| names                         | case insensitive: two names that differ only in case are an error, as are `" * / : < > ? \ \|` and control characters |
| size limits                   | files are limited to 4 GiB minus a byte, directories to 65536 entries                             |
| whiteouts (`.wh.`)            | not translated: a FAT32 image is a filesystem, so a `.wh.` entry is an ordinary file             |

## iso9660

An ISO 9660 layer is a `.iso` image: the filesystem of an optical disc, and the shape every installer and
live system is shipped in. Like the other formats, it is written by a pure-Go implementation in the `img`
tool, so building one doesn't require `xorriso`, `genisoimage` or root privileges and works on every
platform rules_img supports (including Windows).

```python
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(
    name = "release",
    srcs = {
        "/README.md": ":readme",
        "/bin/installer": "//cmd/installer",
    },
    format = "iso9660",
)
```

The image is the layer target's `DefaultInfo` (or, with compact layers enabled, its compact stream).
Everything that is not tar-specific works as usual, including compact layers
(`experimental_compact_layers`) and the `mtree` output group. The options that only apply to tar layers,
including `compress`, are ignored. The file extension is `.iso`.

The image is laid out completely before the first byte is written. The volume descriptors, the directory
extents, the extension metadata and the boot catalog come first, followed by the data of every regular
file in the order the files were added, each in one contiguous extent that starts at a 2048 byte block
boundary. A file can therefore be located from its offset and size alone, which is what makes compact
streams of ISO 9660 images efficient: every file of at least the inline threshold becomes a reference to
the content-addressed store. All-zero blocks are never written, and file contents are not held in memory
while the image is built: they are read from where they are when the image is written.

Everything about the image is a pure function of its inputs: timestamps default to the Unix epoch, the
UDF volume set identifier is derived from the volume identifier, and the layout depends only on the
content.

### Extensions

Plain ISO 9660 can only store upper case, 8.3-ish names and no POSIX metadata at all, so the interesting
part of the format is its extensions. Each one is optional, they combine, and all of them describe the
same file data, which is stored exactly once:

| Extension      | What it adds                                                                                      | Who reads it     |
|----------------|---------------------------------------------------------------------------------------------------|------------------|
| Rock Ridge     | the real names, the mode, the owner, the timestamps, symbolic links, device nodes, FIFOs, hard links | Linux            |
| Joliet         | a second directory hierarchy with UCS-2 names                                                      | Windows          |
| UDF            | a third, ECMA-167 hierarchy with long names, POSIX metadata and no 4 GiB file size limit            | macOS, Windows   |

Rock Ridge (SUSP 1.12 plus RRIP 1.12) is **on by default**, because without it a layer loses all of its
metadata and a layer with a symbolic link, a device node or a FIFO fails outright. Joliet and UDF are off
by default, because enabling them is a decision about who reads the image. They come from an
`iso9660_config` target passed to the layer's `format_config` attribute:

```python
load("@rules_img//img:layer.bzl", "image_layer", "iso9660_config")

iso9660_config(
    name = "release_config",
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

The global `--@rules_img//img/settings:iso9660_rock_ridge` setting flips the Rock Ridge default for every
layer; `rock_ridge = "disabled"` on a single config does it for one.

`rock_ridge = "rationalized"` additionally normalizes the metadata of every entry, the way
`mkisofs -rational-rock` does: the owner becomes root, the mode becomes read-only for everyone, the
execute bit is kept and shared by all three classes, and the setuid, setgid and sticky bits are cleared.
It makes an image independent of whoever built it, and it applies to the UDF metadata as well, because
that describes the same entries.

UDF is written at revision 1.02, in the layout known as a UDF bridge image: the ISO 9660 volume
descriptors come first and the UDF volume recognition sequence continues them, so a reader of either
format finds what it looks for. It costs one 2 KiB block per file and directory for its File Entry, plus
the blocks of the directories' file identifier descriptors.

Regular files with the same content and metadata share one extent, which with Rock Ridge makes them hard
links of each other:

```
# ISO 9660 deduplication is enabled by default.
# This setting disables deduplication and gives each input its own extent.
common --@rules_img//img/settings:iso9660_deduplicate=disabled
```

### Making the image bootable

El Torito points firmware at boot images that are ordinary files of the filesystem. Each entry is an
`iso9660_boot_entry` target listed in the `boot` attribute of an `iso9660_config`, in the order the boot
catalog should have them: the first becomes the default entry and every further one becomes a section of
its own, so a single image boots on BIOS and on UEFI.

```python
load("@rules_img//img:layer.bzl", "image_layer", "iso9660_boot_entry", "iso9660_config")

# A BIOS boot sector. isolinux reads its own location out of the boot
# information table, which the image writer patches in.
iso9660_boot_entry(
    name = "bios_boot",
    boot_info_table = True,
    image = "/isolinux/isolinux.bin",
    load_size = 4,
    platform = "bios",
)

# A UEFI firmware mounts the boot image as a FAT file system and reads the boot
# loader out of it, so the boot image is itself an image_layer.
image_layer(
    name = "esp",
    srcs = {"/EFI/BOOT/BOOTX64.EFI": ":bootloader"},
    format = "fat32",
)

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
    volume_id = "MYPRODUCT_INSTALL",
)

image_layer(
    name = "installer",
    srcs = {
        "/isolinux/isolinux.bin": ":isolinux",
        "/EFI/BOOT/efiboot.img": ":esp",
        "/boot/vmlinuz": ":kernel",
    },
    format = "iso9660",
    format_config = ":installer_config",
)
```

A boot image whose boot information table is patched in has a different content than the file that went
in, so it is always carried in the image itself rather than becoming a reference in a compact stream, and
it may not share its extent with another entry.

Making the image bootable from a USB stick as well (an isohybrid MBR in the system area) is not
supported.

### Files larger than 4 GiB

A directory record describes an extent with a 32 bit length, so a file larger than that is recorded in
several extents: one directory record per extent, all with the same identifier and all but the last marked
as continuing. This is what ISO 9660 interchange level 3 means, and it is what `xorriso -iso-level 3`
produces. Nothing has to be enabled for it.

It is worth knowing what the alternative looks like, because it is what `mkisofs`/`genisoimage` do:
they have no multi-extent support at all, so a large file either aborts the build

```
-allow-limited-size was not specified. There is no way do represent this file size. Aborting.
```

or, with `-allow-limited-size`, is recorded with a **wrong** size (the real size modulo 4 GiB) in the
ISO 9660, Rock Ridge and Joliet views, leaving UDF as the only view where the file is intact. rules_img
never does that: every view reports the real size.

The extents of a file are contiguous, so a file is still one run of bytes in the image and a compact
stream references it as a single range.

One caveat for Joliet: multi-extent is spec-correct in the Joliet hierarchy too, but not every reader
honours the flag there. If a consumer reads a larger-than-4 GiB file through Joliet rather than through
Rock Ridge or UDF, check it.

### Limits

| Feature                        | Status                                                                                                  |
|--------------------------------|---------------------------------------------------------------------------------------------------------|
| file size                      | no 4 GiB limit: a larger file is recorded in several extents (interchange level 3), like `xorriso -iso-level 3`. With UDF enabled, a single file is limited to about 250 GiB, because its allocation descriptors have to fit in one File Entry |
| directory depth                | not limited to the eight levels ISO 9660 allows; deep directories are not relocated, which every current reader copes with |
| names without an extension     | mangled to upper case d-characters (`A-Z 0-9 _`) with a `;1` version, which only a reader that uses no extension sees |
| extended attributes            | dropped                                                                                                 |
| sockets                        | cannot be added                                                                                         |
| whiteouts (`.wh.`)             | not translated: an ISO 9660 image is a filesystem, so a `.wh.` entry is an ordinary file                |
| multi-session, isohybrid MBR, APM | not written                                                                                          |

## cpio

A [cpio](https://docs.kernel.org/driver-api/early-userspace/buffer-format.html) layer is an archive in the
"newc" format, which is what the Linux kernel unpacks into its root file system as an initramfs (initrd).
Like ext4 images, the archives are written by a pure-Go implementation in the `img` tool, so building one
doesn't require `cpio` or root privileges and works on every platform rules_img supports (including Windows).

```python
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(
    name = "initramfs",
    srcs = {
        "/init": "//cmd/init",
        "/etc/hostname": ":hostname",
    },
    compress = "zstd",
    format = "cpio",
)
```

The archive is the layer target's `DefaultInfo` (or, with compact layers enabled,
its compact stream). Everything that is not tar-specific works as usual,
including compact layers (`experimental_compact_layers`) and the `mtree` output
group.

Unlike a filesystem image, an initramfs is usually compressed. The `compress` attribute applies to
`cpio` layers, and the kernel unpacks `gzip` and `zstd` archives as they are. The default is to leave the
archive uncompressed (the global `--@rules_img//img/settings:compress` default is for tar layers).
The file extension is `.cpio`, `.cpio.gz` or `.cpio.zst`. With compact layers, the compact stream describes the
uncompressed archive and records how to compress it again, so the layer file is reconstructed byte for byte.

Entries are written in a deterministic order, directories before their content. The kernel does not create
missing parent directories, so directories that nothing describes are added with mode 0755 and
owned by root. Symbolic links, device nodes, FIFOs, hard links and ownership are kept.
Regular files with the same content are automatically deduplicated using hard links
of each other, just like in a tar layer. Following GNU cpio, only the last member of a set of
hard links carries the content, which the kernel handles when it unpacks the archive:

```
# cpio deduplication is enabled by default.
# This setting disables deduplication and stores each input as a regular file.
common --@rules_img//img/settings:cpio_deduplicate=disabled
```

The same can be configured per layer with a `cpio_config` target passed to the layer's `format_config` attribute:

```python
load("@rules_img//img:layer.bzl", "cpio_config", "image_layer")

cpio_config(
    name = "initramfs_config",
    deduplicate = "disabled",
)

image_layer(
    name = "initramfs",
    srcs = {"/init": "//cmd/init"},
    format = "cpio",
    format_config = ":initramfs_config",
)
```

File contents are not held in memory while the archive is built: they are read from where they are when the
archive is written.

The format stores sizes, ownership, timestamps (seconds since the epoch) and device numbers in 32 bits.
A value that does not fit is an error rather than silently truncated.

| Feature                 | Status                                                                               |
|-------------------------|--------------------------------------------------------------------------------------|
| files larger than 4 GiB | not supported by the format (the limit is 4 GiB minus a byte)                        |
| extended attributes     | dropped                                                                              |
| sockets                 | cannot be added                                                                      |
| whiteouts (`.wh.`)      | not translated: a cpio archive is a file tree, so a `.wh.` entry is an ordinary file |
| CRC format (`070702`)   | not written; the kernel reads the `070701` format                                    |
