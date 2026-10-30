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
| `cpio`   | cpio archive (initramfs)                  | **no**                            | `.cpio`, `.cpio.gz`, `.cpio.zst` |

`tar` is the default, and it is the only format currently specified in the
[OCI image spec](https://github.com/opencontainers/image-spec/blob/main/layer.md).
Every registry and runtime understands it, so unless you have a specific reason
to pick something else, pick tar. [EROFS](erofs-layers.md) is emerging as an
alternative on the wire. Containerd can mount such a layer directly instead of
unpacking it. `ext4` and `cpio` are not valid OCI layer formats:
nothing in the ecosystem expects an ext4 blob or a cpio archive in an image manifest.
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
