"""Rules to convert uncompressed Docker save or OCI layout tar archives to image manifests or indexes."""

load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:layer_helper.bzl", "compression_tuning_args")
load("//img/private/conversion:common.bzl", "CONVERSION_PRIVATE_ATTRS", "PLAIN_TAR_FORMAT", "SUPPORTED_ARCHITECTURES", "SUPPORTED_OSES", "build_manifest_info", "declare_manifest_outputs", "index_providers", "make_layer_infos", "manifest_providers", "normalize_variant", "parse_platform_spec", "validate_and_resolve_compression")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")

def _image_manifest_from_archive_impl(ctx):
    src = ctx.file.src
    if src.is_directory:
        fail("image_manifest_from_archive expects a .tar file, got directory '{}'. Use image_manifest_from_oci_layout instead.".format(src.path))

    arch = ctx.attr.architecture
    os = ctx.attr.os
    variant = normalize_variant(arch, ctx.attr.variant)
    layer_mts = ctx.attr.layers
    uniform_comp = validate_and_resolve_compression(layer_mts)

    outs = declare_manifest_outputs(ctx, layer_mts)
    args = ctx.actions.args()
    args.add("manifest-from-archive")
    args.add("--src", src)
    args.add("--manifest", outs.manifest)
    args.add("--config", outs.config)
    args.add("--descriptor", outs.descriptor)
    args.add("--digest", outs.digest)
    args.add("--architecture", arch)
    args.add("--os", os)
    if variant != "":
        args.add("--variant", variant)
    if uniform_comp != None:
        args.add_all(compression_tuning_args(ctx, uniform_comp, PLAIN_TAR_FORMAT))

    for i, mt in enumerate(layer_mts):
        args.add("--layer_media_type={}={}".format(i, mt))
        args.add(outs.blobs[i], format = "--layer_blob={}=%s".format(i))
        args.add(outs.metadatas[i], format = "--layer_metadata_json={}=%s".format(i))

    ctx.actions.run(
        inputs = [src],
        outputs = outs.all_files,
        arguments = [args],
        executable = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo.tool_exe,
        mnemonic = "ConvertArchiveToImageManifest",
        progress_message = "Converting archive {} to image manifest {}".format(src.short_path, outs.manifest.short_path),
    )

    layer_infos = make_layer_infos(ctx, ctx.attr.name, layer_mts, outs.blobs, outs.metadatas)
    manifest_info = build_manifest_info(ctx, outs, layer_infos, os = os, architecture = arch, variant = variant)
    return manifest_providers(ctx, manifest_info, outs.digest)

def _image_index_from_archive_impl(ctx):
    src = ctx.file.src
    if src.is_directory:
        fail("image_index_from_archive expects a .tar file, got directory '{}'. Use image_index_from_oci_layout instead.".format(src.path))

    layer_mts = ctx.attr.layers
    uniform_comp = validate_and_resolve_compression(layer_mts)
    if len(ctx.attr.manifests) == 0:
        fail("At least one manifest platform must be specified.")
    platforms = [parse_platform_spec(p) for p in ctx.attr.manifests]

    output_index = ctx.actions.declare_file(ctx.attr.name + "_index.json")
    output_digest = ctx.actions.declare_file(ctx.attr.name + "_digest")
    output_descriptor = ctx.actions.declare_file(ctx.attr.name + "_descriptor.json")
    outputs = [output_index, output_digest, output_descriptor]

    args = ctx.actions.args()
    args.add("index-from-archive")
    args.add("--src", src)
    args.add("--index", output_index)
    args.add("--digest", output_digest)
    args.add("--index-descriptor", output_descriptor)
    if uniform_comp != None:
        args.add_all(compression_tuning_args(ctx, uniform_comp, PLAIN_TAR_FORMAT))

    manifest_infos = []
    for idx, (os, arch, variant) in enumerate(platforms):
        suffix = "_{}".format(idx)
        m_outs = declare_manifest_outputs(ctx, layer_mts, suffix = suffix, include_digest = False)
        outputs.extend(m_outs.all_files)

        args.add(m_outs.manifest, format = "--manifest={}=%s".format(idx))
        args.add(m_outs.config, format = "--config={}=%s".format(idx))
        args.add(m_outs.descriptor, format = "--descriptor={}=%s".format(idx))
        args.add("--os={}={}".format(idx, os))
        args.add("--architecture={}={}".format(idx, arch))
        if variant != "":
            args.add("--variant={}={}".format(idx, variant))

        for i, mt in enumerate(layer_mts):
            args.add("--layer_media_type={},{}={}".format(idx, i, mt))
            args.add(m_outs.blobs[i], format = "--layer_blob={},{}=".format(idx, i) + "%s")
            args.add(m_outs.metadatas[i], format = "--layer_metadata_json={},{}=".format(idx, i) + "%s")

        layer_infos = make_layer_infos(ctx, ctx.attr.name + suffix, layer_mts, m_outs.blobs, m_outs.metadatas)
        manifest_infos.append(build_manifest_info(ctx, m_outs, layer_infos, os = os, architecture = arch, variant = variant, suffix = suffix))

    ctx.actions.run(
        inputs = [src],
        outputs = outputs,
        arguments = [args],
        executable = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo.tool_exe,
        mnemonic = "ConvertArchiveToImageIndex",
        progress_message = "Converting archive {} to image index {}".format(src.short_path, output_index.short_path),
    )

    return index_providers(ctx, output_index, output_descriptor, output_digest, manifest_infos)

image_manifest_from_archive = rule(
    implementation = _image_manifest_from_archive_impl,
    doc = """Converts an uncompressed Docker save or OCI layout tar archive to a single-platform image manifest.

Use this rule when the archive layer count is known at Bazel analysis time and you want to preserve individual layers. When the archive layer count is unknown or you want a single squashed layer, use `image_flatten` instead.
""",
    attrs = dict(
        CONVERSION_PRIVATE_ATTRS,
        src = attr.label(
            doc = "Uncompressed Docker save or OCI layout `.tar` archive to convert.",
            mandatory = True,
            allow_single_file = True,
        ),
        architecture = attr.string(
            doc = "Target CPU architecture for the image manifest.",
            mandatory = True,
            values = SUPPORTED_ARCHITECTURES,
        ),
        os = attr.string(
            doc = "Target operating system for the image manifest.",
            mandatory = True,
            values = SUPPORTED_OSES,
        ),
        layers = attr.string_list(
            doc = "Ordered list of layer media types. Use the constants from `@rules_img//img:media_types.bzl`.",
            mandatory = True,
        ),
        variant = attr.string(
            doc = "Platform variant, for example 'v3' for amd64 or 'v8' for arm64.",
            default = "",
        ),
    ),
    provides = [ImageManifestInfo],
    toolchains = TOOLCHAINS,
)

image_index_from_archive = rule(
    implementation = _image_index_from_archive_impl,
    doc = """Converts an uncompressed multi-platform OCI layout or Docker save tar archive to an image index.

Use this rule when every platform manifest in the archive has the same known layer count at Bazel analysis time. When layer counts are unknown or vary across platforms, use `image_flatten` with `manifests` instead.
""",
    attrs = dict(
        CONVERSION_PRIVATE_ATTRS,
        src = attr.label(
            doc = "Uncompressed multi-platform OCI layout or Docker save `.tar` archive to convert.",
            mandatory = True,
            allow_single_file = True,
        ),
        layers = attr.string_list(
            doc = "Ordered list of layer media types applied to each manifest. Use the constants from `@rules_img//img:media_types.bzl`.",
            mandatory = True,
        ),
        manifests = attr.string_list(
            doc = """Ordered list of platform specifications in 'os/architecture' or 'os/architecture/variant' format.
            Example: ["linux/arm64", "linux/amd64/v3"]""",
            mandatory = True,
        ),
    ),
    provides = [ImageIndexInfo],
    toolchains = TOOLCHAINS,
)
