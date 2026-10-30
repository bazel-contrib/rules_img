"""Layer rule for converting existing tar files to usable layers."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:layer_helper.bzl", "allow_tar_files", "build_layer_mtree", "calculate_layer_info", "extension_to_compression", "format_config_args", "optimize_layer", "recompress_layer", "resolve_format_config", "resolve_tar_format")
load("//img/private/common:media_types.bzl", "CPIO_GZIP_LAYER", "CPIO_LAYER", "CPIO_ZSTD_LAYER", "EROFS_LAYER", "EROFS_ZSTD_LAYER", "EXT4_LAYER")
load("//img/private/providers:format_config_info.bzl", "CpioConfigInfo", "ErofsConfigInfo", "Ext4ConfigInfo")
load("//img/private/providers:layers_info.bzl", "LayersInfo")

def _layer_from_tar_impl(ctx):
    optimize = ctx.attr.optimize
    source_compression = extension_to_compression[ctx.file.src.extension]
    compression = ctx.attr.compress
    explicit_target_compression = compression != "auto"
    if compression == "auto":
        compression = ctx.attr._default_compression[BuildSettingInfo].value

    layer_format = ctx.attr.format
    if layer_format == "auto":
        layer_format = ctx.attr._default_layer_format[BuildSettingInfo].value
    is_erofs = layer_format == "erofs"
    is_ext4 = layer_format == "ext4"
    is_cpio = layer_format == "cpio"

    if is_ext4:
        # Like an EROFS layer, an ext4 layer is rebuilt from the tar entry by
        # entry. It is never compressed, and the options that only apply to tar
        # layers are ignored rather than rejected, as for a built ext4 layer.
        compression = "none"

    if is_cpio and not explicit_target_compression:
        # An initramfs is left uncompressed unless asked otherwise. Like an EROFS
        # layer, it is rebuilt from the tar entry by entry, so the source
        # compression is of no use; the options that only apply to tar layers
        # are ignored rather than rejected.
        compression = "none"

    if is_erofs:
        # An EROFS layer is a different container format, not a different
        # compression of the same bytes, so the source blob can never be reused:
        # the tar is always read back entry by entry and rebuilt as a filesystem
        # image. The compression default follows the same reasoning as for a
        # built EROFS layer -- uncompressed, so the blob is mountable as it is.
        if not explicit_target_compression:
            compression = "none"
        if compression == "gzip":
            fail(("format = \"erofs\" cannot be gzip-compressed: there is no " +
                  "application/vnd.erofs.layer.v1+gzip media type. Set compress = \"none\" or compress = \"zstd\"."))

    tar_format = resolve_tar_format(ctx, compression, erofs = is_erofs, ext4 = is_ext4, cpio = is_cpio)
    tar_format_enabled = tar_format.estargz or tar_format.zstd_chunked

    target_compression = compression if (explicit_target_compression or tar_format_enabled or optimize or is_erofs or is_ext4 or is_cpio) else source_compression

    needs_recompression = source_compression != target_compression
    needs_rewrite = needs_recompression or optimize or is_erofs or is_ext4 or is_cpio or tar_format_enabled

    media_type = ctx.attr.media_type
    if media_type == "":
        if is_ext4:
            media_type = EXT4_LAYER
        elif is_cpio:
            media_type = {"gzip": CPIO_GZIP_LAYER, "zstd": CPIO_ZSTD_LAYER}.get(target_compression, CPIO_LAYER)
        elif is_erofs:
            media_type = EROFS_ZSTD_LAYER if target_compression == "zstd" else EROFS_LAYER
        else:
            media_type = "application/vnd.oci.image.layer.v1.tar"
            if target_compression != "none":
                media_type += "+{}".format(target_compression)

    metadata_file = ctx.actions.declare_file("{}_metadata.json".format(ctx.attr.name))
    if is_ext4:
        output_name_extension = ".ext4"
    elif is_cpio:
        output_name_extension = {"gzip": ".cpio.gz", "zstd": ".cpio.zst"}.get(target_compression, ".cpio")
    elif is_erofs:
        output_name_extension = ".erofs.zst" if target_compression == "zstd" else ".erofs"
    elif target_compression == "gzip":
        output_name_extension = ".tgz"
    elif target_compression == "zstd":
        output_name_extension = ".tar.zst"
    elif target_compression == "none":
        output_name_extension = ".tar"
    else:
        fail("Unsupported compression algorithm: {}".format(target_compression))

    if not needs_rewrite:
        # here, we can simply calculate the layer info (size, digest, etc.) and return.
        # The blob is the source tar as-is, so build its mtree here and pass it in.
        layer_info = calculate_layer_info(
            ctx = ctx,
            media_type = media_type,
            tar_file = ctx.file.src,
            metadata_file = metadata_file,
            estargz = False,
            annotations = ctx.attr.annotations,
            mtree = build_layer_mtree(ctx, ctx.attr.name, tar_blob = ctx.file.src),
        )
    elif not optimize and not is_erofs and not is_ext4 and not is_cpio:
        # here, we recompress the tar file and calculate the layer info
        # (recompress_layer builds the mtree from the recompressed blob).
        layer_info = recompress_layer(
            ctx = ctx,
            media_type = media_type,
            tar_file = ctx.file.src,
            metadata_file = metadata_file,
            output = ctx.actions.declare_file(ctx.attr.name + output_name_extension),
            target_compression = target_compression,
            tar_format = tar_format,
            annotations = ctx.attr.annotations,
        )
    else:
        # here, we rewrite the tar entry by entry -- deduplicating it, converting
        # it to a filesystem image (EROFS or ext4) or a cpio archive, or both -- and calculate the
        # layer info (optimize_layer builds the mtree from the rewritten blob).
        layer_info = optimize_layer(
            ctx = ctx,
            media_type = media_type,
            tar_file = ctx.file.src,
            metadata_file = metadata_file,
            output = ctx.actions.declare_file(ctx.attr.name + output_name_extension),
            target_compression = target_compression,
            tar_format = tar_format,
            annotations = ctx.attr.annotations,
            layer_format = layer_format,
            format_args = format_config_args(layer_format, resolve_format_config(ctx, layer_format)),
        )

    return [
        DefaultInfo(
            files = depset([layer_info.blob, layer_info.metadata]),
        ),
        OutputGroupInfo(
            layer = depset([layer_info.blob]),
            metadata = depset([layer_info.metadata]),
            mtree = depset([layer_info.mtree]),
        ),
        LayersInfo(layers = [layer_info]),
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

### Output groups

- `mtree`: a single [mtree](https://man.freebsd.org/cgi/man.cgi?mtree(5)) text file
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
        "zstd_chunked": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = """Whether to use the zstd:chunked format. If set to 'auto', uses the global default zstd_chunked setting.
When enabled, the layer is written with content-defined chunks and a table of contents, so
compatible runtimes (podman, CRI-O) can fetch and deduplicate individual chunks.
Requires zstd compression and cannot be combined with estargz.""",
        ),
        "format": attr.string(
            default = "auto",
            values = ["auto", "tar", "erofs", "ext4", "cpio"],
            doc = """Container format of the layer blob. If set to 'auto', uses the global default
--@rules_img//img/settings:layer_format setting.

'erofs' converts the tar into a mountable EROFS filesystem image. That always rewrites the
layer (a format change cannot preserve the source blob) and defaults to `compress = "none"`,
since the resulting blob is mountable as it is. See docs/erofs-layers.md for what a
filesystem image means for the layers below it.

'ext4' converts the tar into an ext4 filesystem image the same way (always rewritten, never
compressed). Like every ext4 layer, it is not a valid OCI layer: use it on its own. The options
that only apply to tar layers (`compress`, `estargz`, `zstd_chunked`) are ignored. See docs/filesystems.md.

'cpio' converts the tar into a cpio archive in the newc format (an initramfs) the same way. It is
left uncompressed unless `compress` asks for `gzip` or `zstd`, which the kernel unpacks as it is.
Like an ext4 layer, it is not a valid OCI layer, and `estargz` and `zstd_chunked` are ignored.""",
        ),
        "format_config": attr.label(
            providers = [[Ext4ConfigInfo], [CpioConfigInfo], [ErofsConfigInfo]],
            doc = """Options of the filesystem image the layer is written as: an `ext4_config` target
for `format = "ext4"`, a `cpio_config` target for `format = "cpio"`, or an `erofs_config` target
for `format = "erofs"`.

Without it, every option of the format is left at its default. A config for a format other
than the layer's own is a configuration error, since it would silently have no effect.""",
        ),
        "annotations": attr.string_dict(
            default = {},
            doc = """Annotations to add to the layer metadata as key-value pairs.""",
        ),
        "media_type": attr.string(
            default = "",
            doc = """Override layer media type. Use e.g. \"application/vnd.cncf.helm.chart.content.v1.tar\" for Helm charts.""",
        ),
        "_default_compression": attr.label(
            default = Label("//img/settings:compress"),
            providers = [BuildSettingInfo],
        ),
        "_default_estargz": attr.label(
            default = Label("//img/settings:estargz"),
            providers = [BuildSettingInfo],
        ),
        "_default_zstd_chunked": attr.label(
            default = Label("//img/settings:zstd_chunked"),
            providers = [BuildSettingInfo],
        ),
        "_default_layer_format": attr.label(
            default = Label("//img/settings:layer_format"),
            providers = [BuildSettingInfo],
        ),
        "_erofs_inline_threshold": attr.label(
            default = Label("//img/settings:erofs_inline_threshold"),
            providers = [BuildSettingInfo],
        ),
        "_cpio_deduplicate": attr.label(
            default = Label("//img/settings:cpio_deduplicate"),
            providers = [BuildSettingInfo],
        ),
        "_ext4_deduplicate": attr.label(
            default = Label("//img/settings:ext4_deduplicate"),
            providers = [BuildSettingInfo],
        ),
        "_erofs_synthetic_directories": attr.label(
            default = Label("//img/settings:erofs_synthetic_directories"),
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
        "_mtree_path_prefix": attr.label(
            default = Label("//img/settings:mtree_path_prefix"),
            providers = [BuildSettingInfo],
        ),
        "_mtree_options": attr.label(
            default = Label("//img/settings:mtree_options"),
            providers = [BuildSettingInfo],
        ),
        "_mtree_layer_layout": attr.label(
            default = Label("//img/settings:mtree_layer_layout"),
            providers = [BuildSettingInfo],
        ),
    },
    toolchains = TOOLCHAINS,
    provides = [LayersInfo],
)
