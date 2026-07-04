"""Image optimization rule for rewriting existing image layers."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:layer_helper.bzl", "compression_tuning_args")
load("//img/private/common:transitions.bzl", "reset_platform_transition")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:oci_layout_settings_info.bzl", "OCILayoutSettingsInfo")
load("//img/private/providers:single_layer_info.bzl", "SingleLayerInfo")

_MEDIA_TYPES = {
    "gzip": "application/vnd.oci.image.layer.v1.tar+gzip",
    "none": "application/vnd.oci.image.layer.v1.tar",
    "zstd": "application/vnd.oci.image.layer.v1.tar+zstd",
}

_OUTPUT_EXTENSIONS = {
    "gzip": ".tgz",
    "none": ".tar",
    "zstd": ".tar.zst",
}

def _layer_metadata_path(layer):
    return layer.metadata.path

def _manifest_descriptor_path(manifest):
    return manifest.descriptor.path

def _resolved_settings(ctx):
    compression = ctx.attr.compress
    if compression == "auto":
        compression = ctx.attr._default_compression[BuildSettingInfo].value

    estargz = ctx.attr.estargz
    if estargz == "auto":
        estargz = ctx.attr._default_estargz[BuildSettingInfo].value
    estargz_enabled = estargz == "enabled"

    if estargz_enabled and compression != "gzip":
        fail("image_optimize with estargz enabled requires gzip compression, got '{}'".format(compression))

    return struct(
        compression = compression,
        estargz = estargz_enabled,
        media_type = _MEDIA_TYPES[compression],
        output_extension = _OUTPUT_EXTENSIONS[compression],
    )

def _check_layer_blob(ctx, layer, manifest_position, layer_position):
    if layer.blob != None:
        return
    location = "layer[{}]".format(layer_position)
    if manifest_position != None:
        location = "manifest[{}].{}".format(manifest_position, location)
    fail("""image_optimize requires all layer blobs to be available, but {} in {} is shallow.

This rule intentionally does not download missing base-image layers. If the image comes from image_pull, configure that pull to materialize layers eagerly before optimizing it.""".format(location, ctx.attr.image.label))

def _optimize_layer(ctx, layer, settings, manifest_position, layer_position):
    _check_layer_blob(ctx, layer, manifest_position, layer_position)

    prefix = ctx.attr.name
    if manifest_position != None:
        prefix += "_manifest_{}".format(manifest_position)
    prefix += "_layer_{}".format(layer_position)

    output = ctx.actions.declare_file(prefix + settings.output_extension)
    metadata = ctx.actions.declare_file(prefix + "_metadata.json")

    args = ctx.actions.args()
    args.add("compress")
    args.add("--source-metadata", layer.metadata.path)
    args.add("--format", settings.compression)
    if settings.estargz:
        args.add("--estargz")
    args.add("--metadata", metadata.path)
    args.add_all(compression_tuning_args(ctx, settings.compression, settings.estargz))
    args.add(layer.blob.path)
    args.add(output.path)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = [layer.blob, layer.metadata],
        outputs = [output, metadata],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "ImageOptimizeLayer",
    )

    return SingleLayerInfo(
        blob = output,
        metadata = metadata,
        media_type = settings.media_type,
        estargz = settings.estargz,
        compact_stream = None,
        layer_input_files = None,
        layer_input_files_cas = None,
        # Recompression changes the blob digest, so the original upstream sources
        # no longer serve this blob.
        sources = [],
        mtree = None,
    )

def _optimize_layers(ctx, manifest, settings, manifest_position):
    return [
        _optimize_layer(ctx, layer, settings, manifest_position, layer_position)
        for (layer_position, layer) in enumerate(manifest.layers)
    ]

def _optimize_manifest(ctx, manifest, settings, manifest_position = None):
    suffix = "" if manifest_position == None else "_manifest_{}".format(manifest_position)
    layers = _optimize_layers(ctx, manifest, settings, manifest_position)

    manifest_out = ctx.actions.declare_file(ctx.attr.name + suffix + "_manifest.json")
    config_out = ctx.actions.declare_file(ctx.attr.name + suffix + "_config.json")
    descriptor_out = ctx.actions.declare_file(ctx.attr.name + suffix + "_descriptor.json")
    digest_out = ctx.actions.declare_file(ctx.attr.name + suffix + "_digest")

    args = ctx.actions.args()
    args.add("optimize")
    args.add("--source-manifest", manifest.manifest.path)
    args.add("--source-config", manifest.config.path)
    args.add("--source-descriptor", manifest.descriptor.path)
    args.add_all(layers, format_each = "--layer-from-metadata=%s", map_each = _layer_metadata_path, expand_directories = False)
    args.add("--manifest", manifest_out.path)
    args.add("--config", config_out.path)
    args.add("--descriptor", descriptor_out.path)
    args.add("--digest", digest_out.path)

    inputs = [manifest.manifest, manifest.config, manifest.descriptor]
    inputs.extend([layer.metadata for layer in layers])

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [manifest_out, config_out, descriptor_out, digest_out],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "ImageOptimizeManifest",
    )

    return struct(
        info = ImageManifestInfo(
            descriptor = descriptor_out,
            manifest = manifest_out,
            config = config_out,
            structured_config = dict(manifest.structured_config),
            architecture = manifest.architecture,
            os = manifest.os,
            variant = manifest.variant,
            layers = layers,
        ),
        descriptor = descriptor_out,
        digest = digest_out,
    )

def _build_manifest_oci_layout(ctx, format, manifest):
    if format not in ["directory", "tar"]:
        fail('oci layout format must be either "directory" or "tar"')
    if format == "directory":
        oci_layout_output = ctx.actions.declare_directory(ctx.label.name + "_oci_layout")
    else:
        oci_layout_output = ctx.actions.declare_file(ctx.label.name + "_oci_layout.tar")

    args = ctx.actions.args()
    args.add("oci-layout")
    args.add("--format", format)
    args.add("--manifest", manifest.manifest.path)
    args.add("--config", manifest.config.path)
    args.add("--output", oci_layout_output.path)
    if ctx.attr._oci_layout_settings[OCILayoutSettingsInfo].allow_shallow_oci_layout:
        args.add("--allow-missing-blobs")

    inputs = [manifest.manifest, manifest.config]
    for layer in manifest.layers:
        args.add("--layer", "{}={}".format(layer.metadata.path, layer.blob.path))
        inputs.append(layer.metadata)
        inputs.append(layer.blob)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [oci_layout_output],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        env = {"RULES_IMG": "1"},
        mnemonic = "OCIOptimizedLayout",
    )

    return oci_layout_output

def _build_index_oci_layout(ctx, format, index_out, manifests):
    if format not in ["directory", "tar"]:
        fail('oci layout format must be either "directory" or "tar"')
    if format == "directory":
        oci_layout_output = ctx.actions.declare_directory(ctx.label.name + "_oci_layout")
    else:
        oci_layout_output = ctx.actions.declare_file(ctx.label.name + "_oci_layout.tar")

    args = ctx.actions.args()
    args.add("oci-layout")
    args.add("--format", format)
    args.add("--index", index_out.path)
    args.add("--output", oci_layout_output.path)
    if ctx.attr._oci_layout_settings[OCILayoutSettingsInfo].allow_shallow_oci_layout:
        args.add("--allow-missing-blobs")

    inputs = [index_out]
    for manifest in manifests:
        args.add("--manifest-path", manifest.manifest.path)
        args.add("--config-path", manifest.config.path)
        inputs.append(manifest.manifest)
        inputs.append(manifest.config)
        for layer in manifest.layers:
            args.add("--layer", "{}={}".format(layer.metadata.path, layer.blob.path))
            inputs.append(layer.metadata)
            inputs.append(layer.blob)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [oci_layout_output],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        env = {"RULES_IMG": "1"},
        mnemonic = "OCIOptimizedIndexLayout",
    )

    return oci_layout_output

def _image_optimize_impl(ctx):
    settings = _resolved_settings(ctx)

    if ImageManifestInfo in ctx.attr.image:
        optimized = _optimize_manifest(ctx, ctx.attr.image[ImageManifestInfo], settings)
        manifest = optimized.info
        return [
            DefaultInfo(files = depset([manifest.manifest, manifest.config])),
            OutputGroupInfo(
                descriptor = depset([optimized.descriptor]),
                digest = depset([optimized.digest]),
                oci_layout = depset([_build_manifest_oci_layout(ctx, "directory", manifest)]),
                oci_tarball = depset([_build_manifest_oci_layout(ctx, "tar", manifest)]),
            ),
            manifest,
        ]

    index = ctx.attr.image[ImageIndexInfo]
    optimized_manifests = [
        _optimize_manifest(ctx, manifest, settings, manifest_position).info
        for (manifest_position, manifest) in enumerate(index.manifests)
    ]

    index_out = ctx.actions.declare_file(ctx.attr.name + "_index.json")
    descriptor_out = ctx.actions.declare_file(ctx.attr.name + "_descriptor.json")
    digest_out = ctx.actions.declare_file(ctx.attr.name + "_digest")

    args = ctx.actions.args()
    args.add("optimize")
    args.add("--source-index", index.index.path)
    args.add("--source-descriptor", index.descriptor.path)
    args.add_all(optimized_manifests, format_each = "--manifest-descriptor=%s", map_each = _manifest_descriptor_path, expand_directories = False)
    args.add("--index", index_out.path)
    args.add("--descriptor", descriptor_out.path)
    args.add("--digest", digest_out.path)

    inputs = [index.index, index.descriptor]
    inputs.extend([manifest.descriptor for manifest in optimized_manifests])

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [index_out, descriptor_out, digest_out],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "ImageOptimizeIndex",
    )

    index_info = ImageIndexInfo(
        descriptor = descriptor_out,
        index = index_out,
        manifests = optimized_manifests,
    )
    return [
        DefaultInfo(files = depset([index_out])),
        OutputGroupInfo(
            descriptor = depset([descriptor_out]),
            digest = depset([digest_out]),
            oci_layout = depset([_build_index_oci_layout(ctx, "directory", index_out, optimized_manifests)]),
            oci_tarball = depset([_build_index_oci_layout(ctx, "tar", index_out, optimized_manifests)]),
        ),
        index_info,
    ]

image_optimize = rule(
    implementation = _image_optimize_impl,
    doc = """Rewrites every available layer in an image manifest or image index.

This rule applies image-wide layer transformations, such as recompressing every
layer as eStargz. It is intentionally explicit because it requires every input
layer blob to be available to Bazel. Images that were pulled shallowly will fail
analysis instead of downloading missing base-image layers.

Example:

```python
load("@rules_img//img:image.bzl", "image_optimize")

image_optimize(
    name = "base_estargz",
    image = "@ubuntu//:image",
    estargz = "enabled",
)
```
""",
    attrs = {
        "image": attr.label(
            mandatory = True,
            providers = [[ImageManifestInfo], [ImageIndexInfo]],
            doc = "Image manifest or image index to optimize. All layer blobs must be available.",
        ),
        "compress": attr.string(
            default = "auto",
            values = ["auto", "gzip", "zstd", "none"],
            doc = "Compression algorithm to use for rewritten layers. If set to 'auto', uses the global default compression setting.",
        ),
        "estargz": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = "Whether to rewrite layers using eStargz. If set to 'auto', uses the global default eStargz setting.",
        ),
        "_default_compression": attr.label(
            default = Label("//img/settings:compress"),
            providers = [BuildSettingInfo],
        ),
        "_default_estargz": attr.label(
            default = Label("//img/settings:estargz"),
            providers = [BuildSettingInfo],
        ),
        "_compression_jobs": attr.label(
            default = Label("//img/settings:compression_jobs"),
            providers = [BuildSettingInfo],
        ),
        "_compression_level": attr.label(
            default = Label("//img/settings:compression_level"),
            providers = [BuildSettingInfo],
        ),
        "_oci_layout_settings": attr.label(
            default = Label("//img/private/settings:oci_layout"),
            providers = [OCILayoutSettingsInfo],
        ),
    },
    toolchains = TOOLCHAINS,
    cfg = reset_platform_transition,
)
