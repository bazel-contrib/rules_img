"""Layer rule for converting existing tar files to usable layers."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:layer_helper.bzl", "allow_tar_files", "calculate_layer_info", "extension_to_compression", "optimize_layer", "recompress_layer")
load("//img/private/providers:layer_info.bzl", "LayerInfo")

def _layer_from_tar_impl(ctx):
    optimize = ctx.attr.optimize
    source_compression = extension_to_compression[ctx.file.src.extension]
    compression = ctx.attr.compress
    explicit_target_compression = compression != "auto"
    if compression == "auto":
        compression = ctx.attr._default_compression[BuildSettingInfo].value

    estargz = ctx.attr.estargz
    if estargz == "auto":
        estargz = ctx.attr._default_estargz[BuildSettingInfo].value
    estargz_enabled = estargz == "enabled"

    target_compression = compression if (explicit_target_compression or estargz_enabled or optimize) else source_compression

    needs_recompression = source_compression != target_compression
    needs_rewrite = needs_recompression or optimize

    media_type = "application/vnd.oci.image.layer.v1.tar"
    metadata_file = ctx.actions.declare_file("{}_metadata.json".format(ctx.attr.name))
    if target_compression != "none":
        media_type += "+{}".format(target_compression)
    if target_compression == "gzip":
        output_name_extension = ".tgz"
    elif target_compression == "zstd":
        output_name_extension = ".tar.zst"
    elif target_compression == "none":
        output_name_extension = ".tar"
    else:
        fail("Unsupported compression algorithm: {}".format(target_compression))

    if not needs_rewrite:
        # here, we can simply calculate the layer info (size, digest, etc.) and return
        layer_info = calculate_layer_info(
            ctx = ctx,
            media_type = media_type,
            tar_file = ctx.file.src,
            metadata_file = metadata_file,
            estargz = estargz_enabled,
            annotations = ctx.attr.annotations,
        )
    elif not optimize:
        # here, we recompress the tar file and calculate the layer info
        layer_info = recompress_layer(
            ctx = ctx,
            media_type = media_type,
            tar_file = ctx.file.src,
            metadata_file = metadata_file,
            output = ctx.actions.declare_file(ctx.attr.name + output_name_extension),
            target_compression = target_compression,
            estargz = estargz_enabled,
            annotations = ctx.attr.annotations,
        )
    else:
        # here, we optimize, recompress the tar file,
        # and calculate the layer info
        layer_info = optimize_layer(
            ctx = ctx,
            media_type = media_type,
            tar_file = ctx.file.src,
            metadata_file = metadata_file,
            output = ctx.actions.declare_file(ctx.attr.name + output_name_extension),
            target_compression = target_compression,
            estargz = estargz_enabled,
            annotations = ctx.attr.annotations,
        )

    return [
        DefaultInfo(
            files = depset([layer_info.blob, layer_info.metadata]),
        ),
        OutputGroupInfo(
            layer = depset([layer_info.blob]),
            metadata = depset([layer_info.metadata]),
        ),
        layer_info,
    ]

layer_from_tar = rule(
    implementation = _layer_from_tar_impl,
    doc = """Creates a container image layer from an existing tar archive.

This rule converts tar files into container image layers, useful for incorporating
pre-built artifacts, third-party distributions, or legacy build outputs.

The rule can:
- Use tar files as-is or recompress them
- Optimize tar contents by deduplicating files
- Add annotations to the layer metadata

Example:

```python
load("@rules_img//img:layer.bzl", "layer_from_tar")

# Use an existing tar file as a layer
layer_from_tar(
    name = "third_party_layer",
    src = "@third_party_lib//:lib.tar.gz",
)

# Optimize and recompress
layer_from_tar(
    name = "optimized_layer",
    src = "//legacy:build_output.tar",
    optimize = True,  # Deduplicate contents
    compress = "zstd",  # Use zstd compression
)

# Add metadata annotations
layer_from_tar(
    name = "annotated_layer",
    src = "//vendor:dependencies.tar.gz",
    annotations = {
        "org.opencontainers.image.title": "Vendor Dependencies",
        "org.opencontainers.image.version": "1.2.3",
    },
)
```
""",
    attrs = {
        "src": attr.label(
            mandatory = True,
            allow_single_file = allow_tar_files,
            doc = """The tar file to convert into a layer. Must be a valid tar file (optionally compressed).""",
        ),
        "compress": attr.string(
            default = "auto",
            values = ["auto", "gzip", "zstd", "none"],
            doc = """Compression algorithm to use. If set to 'auto', it keeps the existing compression, unless the layer is being optimized.""",
        ),
        "optimize": attr.bool(
            doc = """If set, rewrites the tar file to deduplicate it's contents.
This is useful for reducing the size of the image, but will take extra time and space to store the optimized layer.""",
        ),
        "estargz": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = """Whether to use estargz format. If set to 'auto', uses the global default estargz setting.
When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.""",
        ),
        "annotations": attr.string_dict(
            default = {},
            doc = """Annotations to add to the layer metadata as key-value pairs.""",
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
    },
    toolchains = TOOLCHAINS,
    provides = [LayerInfo],
)
