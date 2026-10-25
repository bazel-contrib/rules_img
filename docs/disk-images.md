# Disk images

`disk_image` assembles filesystem images into a single file with a partition
table: a bootable disk, for a virtual machine, a bare-metal installer or a
firmware that boots from UEFI. The partitions are filesystem images built by
the layer rules ([`image_layer`](layer.md) with `format = "erofs"`,
`"ext4"` or `"fat32"`). The partition table is a GPT (the default) or an MBR.

```python
load("@rules_img//img:disk_image.bzl", "disk_image", "disk_partition")
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(
    name = "esp_layer",
    format = "fat32",
    srcs = {
        "/EFI/BOOT/BOOTX64.EFI": "@systemd_boot//:systemd-bootx64.efi",
        "/loader/loader.conf": ":loader.conf",
    },
)

image_layer(
    name = "root_layer",
    format = "erofs",
    compress = "none",
    srcs = {"/": ":rootfs"},
)

disk_partition(name = "esp", image = ":esp_layer", type = "esp", label = "ESP")
disk_partition(name = "root", image = ":root_layer", type = "root-x86-64", label = "root")

disk_image(
    name = "disk",
    partitions = [":esp", ":root"],
)
```

`bazel build :disk` writes `disk.img`. The reference for the rules is
[disk_image.md](disk_image.md); `img disk-image` is the tool behind them.

## Sizes

By default every size is as small as it can be:

- A partition is as large as its filesystem, padded to the **alignment** (1 MiB
  by default, `alignment`). Partitions start at multiples of the alignment, in
  the order of `partitions`.
- A `disk_partition` with a `partition_size` is that size. A partition that is
  larger than its filesystem has free space at the end. The size has to be at
  least as large as the filesystem.
- The disk ends after the last partition, followed by the backup partition table
  that a GPT keeps at the end of the disk (33 sectors). There is no free space at
  the end other than that.
- `min_size` makes the disk larger (the backup table moves to the new end),
  `max_size` makes it an error if the partitions need more, and `disk_size` is
  both.

A `disk_partition` without an `image` is an empty placeholder of its
`partition_size`, between real filesystems or after the last one. Its type
defaults to `linux`.

The `sector_size` of the disk is 512 (the default) or 4096 bytes. The
alignment is a multiple of it.

## Partition types

`type` is a GUID or one of a list of names (`esp`, `linux`, `root-x86-64`,
`root-arm64`, `home`, `swap`, `xbootldr`, `bios-boot`, ...; see
[disk_image.md](disk_image.md)). The `root-*` types are those of the
[Discoverable Partitions Specification](https://uapi-group.org/specifications/specs/discoverable_partitions_specification/):
systemd finds and mounts such a root filesystem without a kernel command line
argument, and `usr-*`, `home`, `srv`, `var` and `tmp` types are mounted at the
place they belong.

A GPT partition has a `label`, a unique `guid` (derived from the disk if you do
not give one, so that the same inputs always give the same disk image) and
`attributes`. In an MBR, a partition has a type byte (`mbr_type`, derived from
`type` by default) and at most one partition is `active`.

## Bootable UEFI disks

A UEFI firmware looks for an **EFI system partition**: a partition of type
`esp` that holds a FAT32 filesystem. Without a boot entry in the NVRAM of the
machine, it runs the fallback boot loader `/EFI/BOOT/BOOTX64.EFI` (`BOOTAA64.EFI`
on arm64, `BOOTIA32.EFI`, `BOOTRISCV64.EFI`). A GPT disk needs nothing else to
be bootable by UEFI. Build the ESP as a FAT32 layer, as in the example above;
[systemd-boot](https://www.freedesktop.org/software/systemd/man/systemd-boot.html),
GRUB, or a unified kernel image (`/EFI/BOOT/BOOTX64.EFI` itself) are boot loaders
you can put there.

Things to know:

- A FAT32 volume has at least 65525 clusters, so the smallest ESP is about
  33 MiB. The image is a sparse file, but it adds up.
- The hidden sectors field of the FAT32 BPB is set to the first sector of the
  partition (in the boot sector and in the backup boot sector). Some firmware and
  boot loaders need it. Turn it off with `hidden_sectors = False`.
- `type = "esp"` with a filesystem that is not FAT32 is an error.
- On a disk with 4096 byte sectors, build the ESP with the same sector size:
  `image_layer(format = "fat32", sector_size = 4096)`. The smallest ESP is then
  about 256 MiB (65525 clusters of 4096 bytes).
- To boot a GPT disk with a **legacy BIOS**, give `boot_code` (up to 440 bytes of
  boot code, for the protective MBR) and add a partition of type `bios-boot`,
  where the boot loader (GRUB, for example) keeps its second stage. Set the
  `legacy_bios_bootable` attribute on the partition the boot loader reads. For an
  MBR disk, `boot_code` is the MBR boot code and the `active` partition boots.
- Hybrid MBRs (an MBR and a GPT that describe the same partitions) and extended
  partitions are not supported.

## MBR

`partition_table = "mbr"` writes a classic partition table: four primary
partitions with 32 bit start sectors and sizes, which limits a disk to 2 TiB
with 512 byte sectors and 16 TiB with 4096 byte sectors. GUIDs, labels and
attributes only exist in a GPT; setting them with an MBR is an error.

## Compact streams

A disk image can be huge, and most of it is the content of files that Bazel
already has. With `compact_stream = "enabled"` (or `--@rules_img//img/settings:disk_image_compact_stream=enabled`),
`disk_image` produces a [compact stream](compact-stream.md) of the disk image
instead of the disk image: `disk.cstream`.

How the stream is made depends on what a partition is made of:

- **A layer that was built as a compact stream** (`--@rules_img//img/settings:experimental_compact_layers=enabled`,
  for erofs layers): the compact stream of the layer is spliced in. The
  compact stream of the disk then **references the files of the layer**: the same
  blobs the layer's own stream references. Only the partition table, the
  filesystem metadata, small files and the free space are inline, so the stream
  of a multi-gigabyte disk is a few kilobytes plus the metadata.
- **A filesystem image** (an ext4 or erofs layer in the normal mode, or a file):
  it is referenced as a whole by its digest if it is large
  (`compact_stream_inline_threshold`, 1 MiB by default), and stored inline if it
  is small. FAT32 images are always inline: the hidden sectors in the boot sector
  are patched, and an ESP is small. (Compact streams of ext4 and FAT32 layers are
  not yet produced by the layer rules.)
- **Zeros** (free space, gaps, the end of the disk) are inline bytes. The stream
  is compressed as it is written, so they cost compression time, not space. A disk
  with a huge `min_size` takes a while to produce.

The outputs of a `disk_image` in compact mode:

| Output group     | Content |
|------------------|---------|
| (default)        | `disk.cstream` |
| `compact_stream` | the same |
| `cas`            | the content-addressed directories (`sha256/<hex>`) with the blobs the stream references: the ones the layers have, and one with the filesystem images that are referenced as a whole |
| `image`          | the disk image itself, **only built when requested**, assembled from the partitions |
| `layout`         | a JSON description of the layout of the disk |

To get the disk image from the stream, for example outside of Bazel:

```
img compact-stream reconstruct --compact-stream disk.cstream \
    --cas-dir disk.cas --cas-dir layer.inputfilecas --output disk.img
```

`--cas-dir` can be repeated; a blob is taken from the first directory that has
it.

The compact stream does not record the digest of the whole disk, because
computing it would need the content of all the files the stream references.
Every blob it references is addressed by its digest, so only the inline bytes
are not checked.

## `img disk-image`

The command behind the rules. Each `--partition` starts a partition, and the
`--partition-*` flags that follow it describe it:

```
img disk-image --output disk.img \
    --partition ESP  --partition-type esp         --partition-image esp.fat32 \
    --partition root --partition-type root-x86-64 --partition-image root.ext4 --partition-size 4GiB
```

With `--compact`, `--output` is a compact stream, and `--output-cas-dir` receives
the images that it references as a whole. A partition can be given as
`--partition-cstream` (the compact stream of the filesystem); to write a disk
image from it, `--cas-dir` has to have the blobs it references. See
`img disk-image --help`.

## Reproducibility

The same inputs give the same disk image, bit for bit: partitions are placed
deterministically, GUIDs and the MBR disk signature are derived from the
specification of the disk unless you set them, filesystem images are
reproducible, and timestamps in the filesystems are fixed.
