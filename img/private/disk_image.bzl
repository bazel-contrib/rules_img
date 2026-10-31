"""Rules for disk images with a GPT or MBR partition table."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:media_types.bzl", "EROFS_LAYER", "EXT4_LAYER", "FAT32_LAYER")
load("//img/private/providers:disk_image_info.bzl", "DiskImageInfo")
load("//img/private/providers:layers_info.bzl", "LayersInfo")
load("//img/private/providers:partition_info.bzl", "PartitionInfo")

_FILESYSTEM_MEDIA_TYPES = [EXT4_LAYER, FAT32_LAYER, EROFS_LAYER]

def _partition_source(ctx):
    """Finds the file system image, or its compact stream, behind the image attribute."""
    target = ctx.attr.image
    if target == None:
        return None, None, []

    if LayersInfo in target:
        layers = target[LayersInfo].layers
        if len(layers) != 1:
            fail("{}: image must be a layer target that produces exactly one layer, but {} produces {}. Rules that split their content into several layers cannot be used for a partition.".format(ctx.label, target.label, len(layers)))
        layer = layers[0]
        if layer.media_type not in _FILESYSTEM_MEDIA_TYPES:
            fail("{}: the layer {} has the media type {}, which is not a file system image. Build it with format = \"ext4\", \"fat32\" or \"erofs\" (and compress = \"none\" for erofs).".format(ctx.label, target.label, layer.media_type))
        if layer.blob == None and layer.compact_stream == None:
            fail("{}: the layer {} has neither a blob nor a compact stream.".format(ctx.label, target.label))
        cas_dirs = [layer.layer_input_files_cas] if layer.layer_input_files_cas != None else []
        return layer.blob, layer.compact_stream, cas_dirs

    files = target[DefaultInfo].files.to_list()
    if len(files) != 1:
        fail("{}: image must be a layer or a target with exactly one file (a file system image), but {} has {} files.".format(ctx.label, target.label, len(files)))
    return files[0], None, []

def _disk_partition_impl(ctx):
    image, compact_stream, cas_dirs = _partition_source(ctx)
    if image == None and compact_stream == None and ctx.attr.partition_size in ("", "auto"):
        fail("{}: a partition without an image is an empty placeholder and needs a size.".format(ctx.label))
    files = [f for f in [image, compact_stream] if f != None]
    return [
        DefaultInfo(files = depset(files)),
        PartitionInfo(
            label = ctx.attr.label or ctx.label.name,
            type = ctx.attr.type,
            guid = ctx.attr.guid,
            size = ctx.attr.partition_size,
            attributes = ctx.attr.attributes,
            mbr_type = ctx.attr.mbr_type,
            active = ctx.attr.active,
            hidden_sectors = ctx.attr.hidden_sectors,
            image = image,
            compact_stream = compact_stream,
            cas_dirs = cas_dirs,
        ),
    ]

disk_partition = rule(
    implementation = _disk_partition_impl,
    doc = """A partition of a `disk_image`.

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
""",
    attrs = {
        "image": attr.label(
            doc = """The file system image of the partition: a layer rule target with a single layer of the `ext4`, `fat32` or `erofs` format, or a target with a single file (a file system image).

If the layer was built as a compact stream (see `experimental_compact_layers`), a `disk_image` that is built as an extent stream references the compact stream of the layer.""",
            allow_files = True,
        ),
        "label": attr.string(
            doc = "Name of the partition, which a GPT stores. Defaults to the name of the target.",
        ),
        "type": attr.string(
            default = "linux",
            doc = """Type of the partition: a name, or a GUID.

The names are `esp` (EFI system partition), `linux`, `root-x86-64`, `root-x86`, `root-arm64`, `root-arm`, `root-riscv64`, `usr-x86-64`, `usr-arm64`, `home`, `srv`, `var`, `tmp`, `swap`, `xbootldr`, `bios-boot`, `linux-lvm`, `linux-raid`, `microsoft-basic-data` and `fat32`. The partition types for the root file system (`root-*`) are those of the Discoverable Partitions Specification, which lets systemd find the root file system without a kernel command line.

An EFI system partition has to hold a FAT32 file system.""",
        ),
        "guid": attr.string(
            doc = "Unique GUID of the partition (GPT only). By default one is derived from the disk, so that equal inputs give equal disk images.",
        ),
        "partition_size": attr.string(
            default = "auto",
            doc = "Size of the partition, like `64MiB` or `1.5GiB` (binary suffixes KiB, MiB, GiB, TiB; KB, MB, GB, TB are decimal). `auto` makes the partition as large as its file system, padded to the alignment of the disk. A larger size leaves free space at the end of the partition. Placeholders need a `partition_size`.",
        ),
        "attributes": attr.string_list(
            doc = "GPT attributes of the partition: `required`, `no_block_io`, `legacy_bios_bootable`, `growfs`, `read_only`, `hidden`, `no_automount`, or a number (a mask of bits). GPT only.",
        ),
        "mbr_type": attr.int(
            doc = "Type byte of the partition in an MBR, like 0x83 (131). By default it follows from `type` (0xEF for `esp`, 0x83 for Linux, 0x0C for `fat32`).",
        ),
        "active": attr.bool(
            doc = "Marks the partition as bootable in an MBR (at most one partition). MBR only.",
        ),
        "hidden_sectors": attr.bool(
            default = True,
            doc = "Sets the hidden sectors field of a FAT32 file system (in its boot sector and in the backup boot sector) to the first sector of the partition. Some firmware and boot loaders use it to find the file system. Turn it off to keep the file system as it was built.",
        ),
    },
)

def _sizes_args(args, ctx):
    args.add("--partition-table", ctx.attr.partition_table)
    args.add("--sector-size", str(ctx.attr.sector_size))
    args.add("--alignment", ctx.attr.alignment)
    if ctx.attr.disk_size:
        args.add("--size", ctx.attr.disk_size)
    if ctx.attr.min_size:
        args.add("--min-size", ctx.attr.min_size)
    if ctx.attr.max_size:
        args.add("--max-size", ctx.attr.max_size)
    if ctx.attr.disk_guid:
        args.add("--disk-guid", ctx.attr.disk_guid)

def _disk_args(ctx, partitions, extent_stream):
    """Returns the arguments of the img tool and the files it reads.

    Regular files are added to the Args as File objects, so that Bazel controls
    how they are spelled; the content-addressed directories are tree artifacts,
    which `Args.add` rejects, so those are added as paths. The actions still do
    not declare `supports-path-mapping`: like `LayerInputFilesCAS`, they read
    (and in extent stream mode write) content-addressed directories whose
    entries can be relative symlinks to input files, and those resolve only at
    unmapped paths.

    Args:
        ctx: rule context.
        partitions: list of PartitionInfo.
        extent_stream: whether the tool writes an extent stream. An extent
            stream references the compact streams of the partitions and does not
            read the files they reference. A disk image is written from the
            images, or from the compact streams and the files they reference.

    Returns:
        tuple of (Args, list of input Files and depsets).
    """
    args = ctx.actions.args()
    args.add("disk-image")
    _sizes_args(args, ctx)
    inputs = []
    if ctx.file.boot_code:
        args.add("--boot-code", ctx.file.boot_code)
        inputs.append(ctx.file.boot_code)

    for p in partitions:
        args.add("--partition", p.label)
        args.add("--partition-type", p.type)
        if p.guid:
            args.add("--partition-guid", p.guid)
        if p.size not in ("", "auto"):
            args.add("--partition-size", p.size)
        for attribute in p.attributes:
            args.add("--partition-attribute", attribute)
        if p.mbr_type:
            args.add("--partition-mbr-type", str(p.mbr_type))
        if p.active:
            args.add("--partition-active")
        if not p.hidden_sectors:
            args.add("--partition-no-hidden-sectors")

        # Use the compact stream where it is referenced (extent stream mode) or
        # where there is no image (it is reconstructed from the blobs).
        use_stream = p.compact_stream != None and (extent_stream or p.image == None)
        if use_stream:
            args.add("--partition-cstream", p.compact_stream)
            inputs.append(p.compact_stream)
            if not extent_stream:
                for cas in p.cas_dirs:
                    args.add("--cas-dir", cas.path)
                    inputs.append(cas)
        elif p.image != None:
            args.add("--partition-image", p.image)
            inputs.append(p.image)
    return args, inputs

def _resolve_extent_stream(ctx):
    setting = ctx.attr.extent_stream
    if setting == "auto":
        setting = ctx.attr._default_extent_stream[BuildSettingInfo].value
    return setting == "enabled"

def _disk_image_impl(ctx):
    partitions = [p[PartitionInfo] for p in ctx.attr.partitions]
    table = ctx.attr.partition_table
    if table == "mbr":
        if len(partitions) > 4:
            fail("{}: an MBR partition table has room for 4 partitions, but there are {}.".format(ctx.label, len(partitions)))
        for p in partitions:
            if p.guid or p.attributes:
                fail("{}: the partition {} has a GUID or attributes, which only a GPT stores. Use partition_table = \"gpt\".".format(ctx.label, p.label))
        if len([p for p in partitions if p.active]) > 1:
            fail("{}: only one partition can be active.".format(ctx.label))
    else:
        for p in partitions:
            if p.active:
                fail("{}: the partition {} is active, which only an MBR stores. Use partition_table = \"mbr\" or the legacy_bios_bootable attribute.".format(ctx.label, p.label))
    if ctx.attr.disk_size and (ctx.attr.min_size or ctx.attr.max_size):
        fail("{}: disk_size cannot be combined with min_size or max_size.".format(ctx.label))

    extent_stream = _resolve_extent_stream(ctx)
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    layout = ctx.actions.declare_file(ctx.attr.name + ".layout.json")
    image = ctx.actions.declare_file(ctx.attr.name + ".img")
    cas_dirs = []
    for p in partitions:
        cas_dirs.extend(p.cas_dirs)

    output_groups = {"layout": depset([layout])}
    if not extent_stream:
        args, inputs = _disk_args(ctx, partitions, extent_stream = False)
        args.add("--layout-json", layout)
        args.add("--output", image)
        ctx.actions.run(
            outputs = [image, layout],
            inputs = depset(inputs),
            executable = img_toolchain_info.tool_exe,
            arguments = [args],
            mnemonic = "DiskImage",
        )
        output_groups["image"] = depset([image])
        return [
            DefaultInfo(files = depset([image])),
            OutputGroupInfo(**output_groups),
            DiskImageInfo(image = image, extent_stream = None, layout = layout, cas_dirs = depset(cas_dirs)),
        ]

    xstream = ctx.actions.declare_file(ctx.attr.name + ".xstream")
    output_cas = ctx.actions.declare_directory(ctx.attr.name + ".cas")
    args, inputs = _disk_args(ctx, partitions, extent_stream = True)
    args.add("--extent-stream")
    args.add("--extent-inline-threshold", ctx.attr.extent_inline_threshold)
    args.add("--output-cas-dir", output_cas.path)
    if img_toolchain_info.use_treeartifact_uplevel_symlinks:
        args.add("--output-cas-symlink")
    args.add("--layout-json", layout)
    args.add("--output", xstream)
    ctx.actions.run(
        outputs = [xstream, output_cas, layout],
        inputs = depset(inputs),
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "DiskImageExtentStream",
    )

    # The disk image itself, only built if something asks for it: it can be
    # huge. It is assembled from the partitions, not from the extent stream, so
    # that it is a sparse file.
    #
    # This plans the disk a second time, and the two plans have to agree, or the
    # image would not be what the extent stream reconstructs. They do because
    # the planner only sees sizes and the partition metadata: the content size
    # of a compact stream is the size of the file system image it describes, and
    # the derived GUIDs hash the same specification either way (the path of the
    # input is not part of it). tests/disk_image compares the two.
    raw_layout = ctx.actions.declare_file(ctx.attr.name + ".image.layout.json")
    raw_args, raw_inputs = _disk_args(ctx, partitions, extent_stream = False)
    raw_args.add("--layout-json", raw_layout)
    raw_args.add("--output", image)
    ctx.actions.run(
        outputs = [image, raw_layout],
        inputs = depset(raw_inputs),
        executable = img_toolchain_info.tool_exe,
        arguments = [raw_args],
        mnemonic = "DiskImage",
    )
    output_groups["image"] = depset([image])
    output_groups["extent_stream"] = depset([xstream])
    output_groups["cas"] = depset([output_cas] + cas_dirs)
    return [
        DefaultInfo(files = depset([xstream])),
        OutputGroupInfo(**output_groups),
        DiskImageInfo(
            image = image,
            extent_stream = xstream,
            layout = layout,
            cas_dirs = depset([output_cas] + cas_dirs),
        ),
    ]

disk_image = rule(
    implementation = _disk_image_impl,
    doc = """A disk image with a GPT (default) or MBR partition table, as a single file.

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

### Extent streams (experimental)

With `extent_stream = "enabled"` (or the `disk_image_extent_stream` setting),
`disk_image` produces an [extent stream](extent-stream.md) of the disk image
instead of the image, which can be huge. The extent stream format is
experimental and may change incompatibly. The layers of the partitions that were
built as compact streams (see `experimental_compact_layers`) are referenced by
the digest of their compact stream, so the extent stream of the disk is tiny:
it embeds the partition table, describes the free space as holes and points at
the partitions. File system images that are not compact streams are referenced
as a whole (or embedded if they are small). The disk image is available in the
`image` output group, and is only built when requested. The `extent_stream` and
`cas` output groups have the extent stream and the content-addressed
directories (`sha256/<hex>`) with the compact streams of the partitions and the
blobs they reference, which `img extent-stream reconstruct --cas-dir` takes.

See [disk images](disk-images.md).
""",
    attrs = {
        "partitions": attr.label_list(
            mandatory = True,
            providers = [PartitionInfo],
            doc = "The `disk_partition` targets, in the order they are laid out on the disk.",
        ),
        "partition_table": attr.string(
            default = "gpt",
            values = ["gpt", "mbr"],
            doc = "The partition table: a GUID partition table (`gpt`), or a classic MBR partition table (`mbr`, at most 4 partitions and 2 TiB with 512 byte sectors).",
        ),
        "sector_size": attr.int(
            default = 512,
            values = [512, 4096],
            doc = "Logical sector size of the disk in bytes. FAT32 file systems in the partitions have to have the same sector size (see the `sector_size` attribute of the layer rules).",
        ),
        "alignment": attr.string(
            default = "1MiB",
            doc = "Alignment of the start of every partition (a multiple of the sector size). The padding is part of the previous partition, and the default of 1 MiB is what current tools use.",
        ),
        "disk_size": attr.string(
            doc = "Exact size of the disk, like `4GiB`. It is an error if the partitions need more. Cannot be combined with `min_size` or `max_size`.",
        ),
        "min_size": attr.string(
            doc = "Minimum size of the disk. If the partitions need less, the disk has free space at the end (the backup partition table of a GPT is at the end of the disk).",
        ),
        "max_size": attr.string(
            doc = "Maximum size of the disk. It is an error if the partitions need more.",
        ),
        "disk_guid": attr.string(
            doc = "GUID of the disk (GPT only). By default one is derived, so that equal inputs give equal disk images. Also the basis of the disk signature of an MBR.",
        ),
        "boot_code": attr.label(
            allow_single_file = True,
            doc = "File with up to 440 bytes of boot code for the first sector: the MBR, or the protective MBR of a GPT (for booting a GPT disk with a legacy BIOS).",
        ),
        "extent_stream": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = "(Experimental) Whether to produce an extent stream of the disk image instead of the disk image. If set to `auto`, uses the global `--@rules_img//img/settings:disk_image_extent_stream` setting.",
        ),
        "extent_inline_threshold": attr.string(
            default = "1MiB",
            doc = "With an extent stream: file system images that are not compact streams and are smaller than this are embedded in the extent stream instead of being referenced.",
        ),
        "_default_extent_stream": attr.label(
            default = Label("//img/settings:disk_image_extent_stream"),
            providers = [BuildSettingInfo],
        ),
    },
    toolchains = TOOLCHAINS,
    provides = [DiskImageInfo],
)
