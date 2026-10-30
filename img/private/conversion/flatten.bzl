"""Rule to flatten all layers of a container image or multi-platform image index into a single layer per platform."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:layer_helper.bzl", "compression_tuning_args", "history_attr", "layer_history_args", "media_type_is_tar")
load("//img/private/common:media_types.bzl", "GZIP_LAYER", "UNCOMPRESSED_LAYER", "ZSTD_LAYER")
load("//img/private/conversion:common.bzl", "CONVERSION_PRIVATE_ATTRS", "PLAIN_TAR_FORMAT", "SUPPORTED_ARCHITECTURES", "SUPPORTED_OSES", "build_manifest_info", "declare_manifest_outputs", "index_providers", "make_layer_infos", "manifest_providers", "normalize_variant", "parse_platform_spec")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")

_MEDIA_TYPES = {
    "gzip": GZIP_LAYER,
    "none": UNCOMPRESSED_LAYER,
    "zstd": ZSTD_LAYER,
}

def _check_layer_blob(ctx, layer, manifest_position, layer_position):
    if layer.blob != None:
        return
    location = "layer[{}]".format(layer_position)
    if manifest_position != None:
        location = "manifest[{}].{}".format(manifest_position, location)
    fail("""image_flatten requires all layer blobs to be available, but {} in {} has no blob.

This rule does not download missing base-image layers. If the image comes from image_pull, configure that pull to materialize layers eagerly before flattening it.

If the layer comes from a local rule built with experimental_compact_layers enabled, build with "--@rules_img//img/settings:experimental_compact_layers_materialize_blob=enabled" to reconstruct the blob on demand.""".format(location, ctx.attr.image.label))

def _check_layer_is_tar(ctx, layer, manifest_position, layer_position):
    if media_type_is_tar(layer.media_type):
        return
    location = "layer[{}]".format(layer_position)
    if manifest_position != None:
        location = "manifest[{}].{}".format(manifest_position, location)
    fail("""image_flatten can only merge tar layers, but {} in {} has media type "{}".

Layers that are not tar changesets (for example a filesystem image built with "--@rules_img//img/settings:layer_format=erofs" or "=ext4") cannot be extracted and would silently flatten into an empty filesystem.""".format(location, ctx.attr.image.label, layer.media_type))

def _has_platform(m):
    # Attestation and unknown-platform manifests (os/architecture "unknown") carry no runnable filesystem.
    return m.os not in ("", "unknown") and m.architecture not in ("", "unknown")

def _flatten_single_manifest(ctx, *, compression, media_type, manifest_position = None, src_manifest = None, src_file = None, os = "", architecture = "", variant = ""):
    suffix = "" if manifest_position == None else "_{}".format(manifest_position)
    outs = declare_manifest_outputs(ctx, [media_type], suffix = suffix)

    args = ctx.actions.args()
    args.add("flatten-image")
    inputs = []

    if src_manifest != None:
        architecture = src_manifest.architecture
        os = src_manifest.os
        variant = src_manifest.variant
        args.add("--source-manifest", src_manifest.manifest)
        args.add("--source-config", src_manifest.config)
        inputs.extend([src_manifest.manifest, src_manifest.config])

        for (i, layer) in enumerate(src_manifest.layers):
            _check_layer_blob(ctx, layer, manifest_position, i)
            _check_layer_is_tar(ctx, layer, manifest_position, i)
            args.add("--layer", layer.blob)
            inputs.append(layer.blob)

        structured_config = dict(src_manifest.structured_config)
        structured_config.pop("history", None)
        structured_config["architecture"] = architecture
        structured_config["os"] = os
        source_desc = str(ctx.attr.image.label)
    else:
        variant = normalize_variant(architecture, variant)
        args.add_all([src_file], before_each = "--src", expand_directories = False)
        args.add("--architecture", architecture)
        args.add("--os", os)
        if variant != "":
            args.add("--variant", variant)
        inputs.append(src_file)
        structured_config = {"architecture": architecture, "os": os}
        source_desc = src_file.short_path

    args.add("--format", compression)
    args.add_all(layer_history_args(ctx))
    args.add_all(compression_tuning_args(ctx, compression, PLAIN_TAR_FORMAT))
    args.add("--manifest", outs.manifest)
    args.add("--config", outs.config)
    args.add("--descriptor", outs.descriptor)
    args.add("--digest", outs.digest)
    args.add("--layer-blob", outs.blobs[0])
    args.add("--layer-metadata", outs.metadatas[0])

    ctx.actions.run(
        inputs = inputs,
        outputs = outs.all_files,
        arguments = [args],
        executable = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo.tool_exe,
        mnemonic = "ImageFlatten",
        progress_message = "Flattening {} into {}".format(source_desc, outs.manifest.short_path),
    )

    layer_infos = make_layer_infos(ctx, ctx.attr.name + suffix, [media_type], outs.blobs, outs.metadatas)
    info = build_manifest_info(
        ctx,
        outs,
        layer_infos,
        os = os,
        architecture = architecture,
        variant = variant,
        structured_config = structured_config,
        suffix = suffix,
    )
    return struct(info = info, digest = outs.digest)

def _build_flattened_index(ctx, flattened_manifests, *, src_index = None):
    index_out = ctx.actions.declare_file(ctx.attr.name + "_index.json")
    descriptor_out = ctx.actions.declare_file(ctx.attr.name + "_descriptor.json")
    digest_out = ctx.actions.declare_file(ctx.attr.name + "_digest")
    descriptors = [m.descriptor for m in flattened_manifests]

    args = ctx.actions.args()
    if src_index != None:
        args.add("optimize")
        args.add("--source-index", src_index.index)
        args.add("--source-descriptor", src_index.descriptor)
        args.add_all(descriptors, format_each = "--manifest-descriptor=%s")
        args.add("--index", index_out)
        args.add("--descriptor", descriptor_out)
        args.add("--digest", digest_out)
        inputs = [src_index.index, src_index.descriptor] + descriptors
    else:
        args.add("index")
        args.add("--descriptor", descriptor_out)
        args.add("--digest", digest_out)
        args.add_all(descriptors, format_each = "--manifest-descriptor=%s")
        args.add(index_out)
        inputs = descriptors

    ctx.actions.run(
        inputs = inputs,
        outputs = [index_out, descriptor_out, digest_out],
        executable = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo.tool_exe,
        arguments = [args],
        mnemonic = "ImageFlattenIndex",
    )
    return index_providers(ctx, index_out, descriptor_out, digest_out, flattened_manifests, mnemonic = "OCIFlattenedIndexLayout")

def _image_flatten_impl(ctx):
    has_image = ctx.attr.image != None
    has_src = ctx.attr.src != None
    if has_image == has_src:
        fail("image_flatten requires exactly one of 'image' or 'src'.")

    compression = ctx.attr.compress
    if compression == "auto":
        compression = ctx.attr._default_compression[BuildSettingInfo].value
    media_type = _MEDIA_TYPES[compression]

    if has_image:
        if len(ctx.attr.manifests) > 0 or ctx.attr.architecture != "" or ctx.attr.os != "" or ctx.attr.variant != "":
            fail("'manifests', 'os', 'architecture', and 'variant' cannot be set when 'image' is used.")

        if ImageManifestInfo in ctx.attr.image:
            res = _flatten_single_manifest(
                ctx,
                compression = compression,
                media_type = media_type,
                src_manifest = ctx.attr.image[ImageManifestInfo],
            )
            return manifest_providers(ctx, res.info, res.digest, mnemonic = "OCIFlattenedLayout")

        src_index = ctx.attr.image[ImageIndexInfo]

        # Attestation manifests are dropped silently; every real platform is flattened and
        # fails loudly in _check_layer_is_tar if its layers are not tar changesets.
        platform_manifests = [m for m in src_index.manifests if _has_platform(m)]
        if len(platform_manifests) == 0:
            fail("image_flatten found no platform manifests in '{}'.".format(ctx.attr.image.label))

        flattened = [
            _flatten_single_manifest(
                ctx,
                compression = compression,
                media_type = media_type,
                manifest_position = pos,
                src_manifest = child,
            ).info
            for (pos, child) in enumerate(platform_manifests)
        ]
        return _build_flattened_index(ctx, flattened, src_index = src_index)

    src_file = ctx.file.src
    if src_file == None:
        fail("src target '{}' produced no files.".format(ctx.attr.src.label))

    if len(ctx.attr.manifests) > 0:
        if ctx.attr.architecture != "" or ctx.attr.os != "" or ctx.attr.variant != "":
            fail("'manifests' cannot be combined with 'os', 'architecture', or 'variant'.")

        platforms = [parse_platform_spec(p) for p in ctx.attr.manifests]
        flattened = [
            _flatten_single_manifest(
                ctx,
                compression = compression,
                media_type = media_type,
                manifest_position = pos,
                src_file = src_file,
                os = plat[0],
                architecture = plat[1],
                variant = plat[2],
            ).info
            for (pos, plat) in enumerate(platforms)
        ]
        return _build_flattened_index(ctx, flattened)

    if ctx.attr.architecture == "" or ctx.attr.os == "":
        fail("image_flatten with 'src' requires either ('os' and 'architecture') for a single-platform image or 'manifests' for a multi-platform index.")

    res = _flatten_single_manifest(
        ctx,
        compression = compression,
        media_type = media_type,
        src_file = src_file,
        os = ctx.attr.os,
        architecture = ctx.attr.architecture,
        variant = ctx.attr.variant,
    )
    return manifest_providers(ctx, res.info, res.digest, mnemonic = "OCIFlattenedLayout")

image_flatten = rule(
    implementation = _image_flatten_impl,
    doc = """Squashes all layers of a container image or multi-platform image index into a single compressed layer per platform and preserves image configuration.

Accepts either an existing `ImageManifestInfo` or `ImageIndexInfo` target via `image`, or an external container archive or OCI layout directory via `src`. Passing `src` lets Bazel import single-platform or multi-platform Docker save and OCI layout tarballs without knowing their layer counts at analysis time.

Example (single-platform archive):

```python
load("@rules_img//img:convert.bzl", "image_flatten")

image_flatten(
    name = "external_base",
    src = ":docker_save_image.tar",
    os = "linux",
    architecture = "amd64",
)
```

Example (multi-platform archive or OCI layout):

```python
load("@rules_img//img:convert.bzl", "image_flatten")

image_flatten(
    name = "external_multiarch_base",
    src = ":multiarch_oci.tar",
    manifests = [
        "linux/amd64",
        "linux/arm64",
    ],
)
```

**Known limitations.** `image_flatten` merges layers with `mutate.Extract` from go-containerregistry. Version v0.22.1 of that library has three limits. Most images do not hit them.

- If a later layer deletes the target of a hardlink, the flattened layer keeps a hardlink to a missing file. Docker then fails to load the image with a `linkat` error. The upstream report uses `RUN rm /usr/bin/git` on Alpine as an example. See [google/go-containerregistry#977](https://github.com/google/go-containerregistry/issues/977).
- If a later layer changes the target of a hardlink, the hardlink gets the new content. In the original image, the hardlink keeps the old content. See [google/go-containerregistry#2002](https://github.com/google/go-containerregistry/issues/2002).
- The library drops relative symlinks and hardlinks that point above the image root, for example `x/link -> ../../etc/file`. It does this on purpose to block path traversal. It keeps absolute links and relative links that stay inside the root. See [google/go-containerregistry#2227](https://github.com/google/go-containerregistry/pull/2227) and [google/go-containerregistry#2279](https://github.com/google/go-containerregistry/pull/2279).
""",
    attrs = dict(
        CONVERSION_PRIVATE_ATTRS,
        image = attr.label(
            providers = [[ImageManifestInfo], [ImageIndexInfo]],
            doc = "Image manifest or multi-platform image index target to flatten. All layer blobs must be available. Mutually exclusive with 'src'.",
        ),
        src = attr.label(
            allow_single_file = True,
            doc = "Uncompressed Docker save `.tar` archive, OCI layout `.tar` archive, or OCI layout directory to flatten. Mutually exclusive with 'image'.",
        ),
        manifests = attr.string_list(
            doc = """Ordered list of platform specifications in 'os/architecture' or 'os/architecture/variant' format when flattening a multi-platform 'src' into an ImageIndexInfo.
            Example: ["linux/amd64", "linux/arm64"]""",
        ),
        architecture = attr.string(
            default = "",
            values = [""] + SUPPORTED_ARCHITECTURES,
            doc = "Target CPU architecture for single-platform 'src' flattening.",
        ),
        os = attr.string(
            default = "",
            values = [""] + SUPPORTED_OSES,
            doc = "Target operating system for single-platform 'src' flattening.",
        ),
        variant = attr.string(
            default = "",
            doc = "Platform variant for single-platform 'src' flattening, for example 'v3' for amd64 or 'v8' for arm64.",
        ),
        compress = attr.string(
            default = "auto",
            values = ["auto", "gzip", "zstd", "none"],
            doc = "Compression algorithm for each flattened layer. When set to 'auto', uses the global default compression setting.",
        ),
        history = history_attr(),
        _default_compression = attr.label(
            default = Label("//img/settings:compress"),
            providers = [BuildSettingInfo],
        ),
    ),
    toolchains = TOOLCHAINS,
)
