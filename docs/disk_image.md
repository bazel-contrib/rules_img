<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for disk images with a GPT or MBR partition table.

<a id="disk_image"></a>

## disk_image

<pre>
load("@rules_img//img:disk_image.bzl", "disk_image")

disk_image(<a href="#disk_image-name">name</a>, <a href="#disk_image-alignment">alignment</a>, <a href="#disk_image-boot_code">boot_code</a>, <a href="#disk_image-compact_stream">compact_stream</a>, <a href="#disk_image-compact_stream_inline_threshold">compact_stream_inline_threshold</a>, <a href="#disk_image-disk_guid">disk_guid</a>,
           <a href="#disk_image-disk_size">disk_size</a>, <a href="#disk_image-max_size">max_size</a>, <a href="#disk_image-min_size">min_size</a>, <a href="#disk_image-partition_table">partition_table</a>, <a href="#disk_image-partitions">partitions</a>, <a href="#disk_image-sector_size">sector_size</a>)
</pre>

A disk image with a GPT (default) or MBR partition table, as a single file.

The partitions are the `disk_partition` targets in `partitions`, in order. Each
holds a file system image, typically the output of `image_layer` with
`format = "ext4"`, `"fat32"` or `"erofs"`. The disk is as small as the
partitions allow (plus the backup partition table of a GPT). Set `min_size`,
`max_size` or `disk_size` to make the disk larger, or to limit its size.

### Bootable UEFI disk image

A UEFI firmware boots from the EFI system partition: a FAT32 file system in a
partition of type `esp`, with the boot loader at `/EFI/BOOT/BOOTX64.EFI`
(`BOOTAA64.EFI` on arm64).

```python
load("@rules_img//img:disk_image.bzl", "disk_image", "disk_partition")
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(
    name = "esp_layer",
    format = "fat32",
    srcs = {
        "/EFI/BOOT/BOOTX64.EFI": "@systemd_boot//:systemd-bootx64.efi",
        "/loader/loader.conf": ":loader.conf",
        "/EFI/Linux/linux.efi": ":unified_kernel_image",
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

### Compact streams

With `compact_stream = "enabled"` (or the `disk_image_compact_stream` setting),
`disk_image` produces a [compact stream](compact-stream.md) of the disk image
instead of the image, which can be huge. The layers of the partitions that were
built as compact streams (see `experimental_compact_layers`) keep their
references to the blobs of their files: the compact stream of the disk
references these files directly, and only the partition table, the file system
metadata and the free space are inline. File system images that are not compact
streams are referenced as a whole (or stored inline if they are small). The disk
image is available in the `image` output group, and is only built when
requested. The `compact_stream` and `cas` output groups have the compact stream
and the content-addressed directories (`sha256/<hex>`) with the blobs it
references, which `img compact-stream reconstruct --cas-dir` takes.

See [disk images](disk-images.md).

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="disk_image-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="disk_image-alignment"></a>alignment |  Alignment of the start of every partition (a multiple of the sector size). The padding is part of the previous partition, and the default of 1 MiB is what current tools use.   | String | optional |  `"1MiB"`  |
| <a id="disk_image-boot_code"></a>boot_code |  File with up to 440 bytes of boot code for the first sector: the MBR, or the protective MBR of a GPT (for booting a GPT disk with a legacy BIOS).   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="disk_image-compact_stream"></a>compact_stream |  Whether to produce a compact stream of the disk image instead of the disk image. If set to `auto`, uses the global `--@rules_img//img/settings:disk_image_compact_stream` setting.   | String | optional |  `"auto"`  |
| <a id="disk_image-compact_stream_inline_threshold"></a>compact_stream_inline_threshold |  With a compact stream: file system images that are not compact streams and are smaller than this are stored inline instead of being referenced as a whole. FAT32 images are always inline: their boot sector is patched.   | String | optional |  `"1MiB"`  |
| <a id="disk_image-disk_guid"></a>disk_guid |  GUID of the disk (GPT only). By default one is derived, so that equal inputs give equal disk images. Also the basis of the disk signature of an MBR.   | String | optional |  `""`  |
| <a id="disk_image-disk_size"></a>disk_size |  Exact size of the disk, like `4GiB`. It is an error if the partitions need more. Cannot be combined with `min_size` or `max_size`.   | String | optional |  `""`  |
| <a id="disk_image-max_size"></a>max_size |  Maximum size of the disk. It is an error if the partitions need more.   | String | optional |  `""`  |
| <a id="disk_image-min_size"></a>min_size |  Minimum size of the disk. If the partitions need less, the disk has free space at the end (the backup partition table of a GPT is at the end of the disk).   | String | optional |  `""`  |
| <a id="disk_image-partition_table"></a>partition_table |  The partition table: a GUID partition table (`gpt`), or a classic MBR partition table (`mbr`, at most 4 partitions and 2 TiB with 512 byte sectors).   | String | optional |  `"gpt"`  |
| <a id="disk_image-partitions"></a>partitions |  The `disk_partition` targets, in the order they are laid out on the disk.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | required |  |
| <a id="disk_image-sector_size"></a>sector_size |  Logical sector size of the disk in bytes. FAT32 file systems in the partitions have to have the same sector size (see the `sector_size` attribute of the layer rules).   | Integer | optional |  `512`  |


<a id="disk_partition"></a>

## disk_partition

<pre>
load("@rules_img//img:disk_image.bzl", "disk_partition")

disk_partition(<a href="#disk_partition-name">name</a>, <a href="#disk_partition-active">active</a>, <a href="#disk_partition-attributes">attributes</a>, <a href="#disk_partition-guid">guid</a>, <a href="#disk_partition-hidden_sectors">hidden_sectors</a>, <a href="#disk_partition-image">image</a>, <a href="#disk_partition-label">label</a>, <a href="#disk_partition-mbr_type">mbr_type</a>,
               <a href="#disk_partition-partition_size">partition_size</a>, <a href="#disk_partition-type">type</a>)
</pre>

A partition of a `disk_image`.

The partition holds a file system image: the output of a layer rule built with
`format = "ext4"`, `"fat32"` or `"erofs"` (see `image_layer`), or any file that
is a file system image. Without an `image`, it is an empty placeholder of the
given `partition_size`.

By default, the partition is as large as the file system, padded to the
alignment of the disk. A larger `partition_size` leaves free space at the end of the
partition.

Example:

```python
load("@rules_img//img:disk_image.bzl", "disk_partition")

disk_partition(
    name = "esp",
    image = ":esp_layer",  # an image_layer with format = "fat32"
    label = "ESP",
    type = "esp",
)

disk_partition(
    name = "root",
    image = ":root_layer",
    label = "root",
    type = "root-x86-64",
    partition_size = "2GiB",  # free space after the file system
)

disk_partition(
    name = "reserved",
    label = "reserved",
    partition_size = "16MiB",  # an empty placeholder
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="disk_partition-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="disk_partition-active"></a>active |  Marks the partition as bootable in an MBR (at most one partition). MBR only.   | Boolean | optional |  `False`  |
| <a id="disk_partition-attributes"></a>attributes |  GPT attributes of the partition: `required`, `no_block_io`, `legacy_bios_bootable`, `growfs`, `read_only`, `hidden`, `no_automount`, or a number (a mask of bits). GPT only.   | List of strings | optional |  `[]`  |
| <a id="disk_partition-guid"></a>guid |  Unique GUID of the partition (GPT only). By default one is derived from the disk, so that equal inputs give equal disk images.   | String | optional |  `""`  |
| <a id="disk_partition-hidden_sectors"></a>hidden_sectors |  Sets the hidden sectors field of a FAT32 file system (in its boot sector and in the backup boot sector) to the first sector of the partition. Some firmware and boot loaders use it to find the file system. Turn it off to keep the file system as it was built.   | Boolean | optional |  `True`  |
| <a id="disk_partition-image"></a>image |  The file system image of the partition: a layer rule target with a single layer of the `ext4`, `fat32` or `erofs` format, or a target with a single file (a file system image).<br><br>If the layer was built as a compact stream (see `experimental_compact_layers`), a `disk_image` that is built as a compact stream keeps the references to the blobs of the files in the layer.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="disk_partition-label"></a>label |  Name of the partition, which a GPT stores. Defaults to the name of the target.   | String | optional |  `""`  |
| <a id="disk_partition-mbr_type"></a>mbr_type |  Type byte of the partition in an MBR, like 0x83 (131). By default it follows from `type` (0xEF for `esp`, 0x83 for Linux, 0x0C for `fat32`).   | Integer | optional |  `0`  |
| <a id="disk_partition-partition_size"></a>partition_size |  Size of the partition, like `64MiB` or `1.5GiB` (binary suffixes KiB, MiB, GiB, TiB; KB, MB, GB, TB are decimal). `auto` makes the partition as large as its file system, padded to the alignment of the disk. A larger size leaves free space at the end of the partition. Placeholders need a `partition_size`.   | String | optional |  `"auto"`  |
| <a id="disk_partition-type"></a>type |  Type of the partition: a name, or a GUID.<br><br>The names are `esp` (EFI system partition), `linux`, `root-x86-64`, `root-x86`, `root-arm64`, `root-arm`, `root-riscv64`, `usr-x86-64`, `usr-arm64`, `home`, `srv`, `var`, `tmp`, `swap`, `xbootldr`, `bios-boot`, `linux-lvm`, `linux-raid`, `microsoft-basic-data` and `fat32`. The partition types for the root file system (`root-*`) are those of the Discoverable Partitions Specification, which lets systemd find the root file system without a kernel command line.<br><br>An EFI system partition has to hold a FAT32 file system.   | String | optional |  `"linux"`  |


