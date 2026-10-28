"""Shared helpers for container archive conversion and layer-flattening rules."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private:oci_layout_action.bzl", "run_oci_layout_action")
load("//img/private/common:layer_helper.bzl", "IMAGE_MTREE_ATTRS", "build_layer_mtree", "image_mtree_or_none")
load("//img/private/common:layout_blob.bzl", "blob_for_layout")
load("//img/private/common:media_types.bzl", "GZIP_LAYER", "LAYER_TYPES", "UNCOMPRESSED_LAYER", "ZSTD_LAYER")
load("//img/private/common:sparse_oci_layout.bzl", "build_sparse_oci_layout_for_index", "build_sparse_oci_layout_for_manifest")
load("//img/private/platforms:matching.bzl", _parse_matching_spec = "parse_platform_spec")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:oci_layout_settings_info.bzl", "OCILayoutSettingsInfo")
load("//img/private/providers:single_layer_info.bzl", "SingleLayerInfo")

SUPPORTED_ARCHITECTURES = [
    "386",
    "amd64",
    "arm",
    "arm64",
    "mips64",
    "ppc64le",
    "riscv64",
    "s390x",
    "wasm",
]

SUPPORTED_OSES = [
    "android",
    "darwin",
    "freebsd",
    "ios",
    "linux",
    "netbsd",
    "openbsd",
    "wasip1",
    "windows",
]

LAYER_EXTENSIONS = {
    UNCOMPRESSED_LAYER: "tar",
    GZIP_LAYER: "tgz",
    ZSTD_LAYER: "tzst",
}

MEDIA_TYPE_TO_COMPRESSION = {
    UNCOMPRESSED_LAYER: "none",
    GZIP_LAYER: "gzip",
    ZSTD_LAYER: "zstd",
}

# Conversion rules emit plain tar layers: estargz and zstd:chunked are lazy-pull
# formats a layer has to be built with, not something a converted blob can gain.
PLAIN_TAR_FORMAT = struct(estargz = False, zstd_chunked = False)

CONVERSION_PRIVATE_ATTRS = dict(
    IMAGE_MTREE_ATTRS,
    _compression_jobs = attr.label(
        default = Label("//img/settings:compression_jobs"),
        providers = [BuildSettingInfo],
    ),
    _compression_level = attr.label(
        default = Label("//img/settings:compression_level"),
        providers = [BuildSettingInfo],
    ),
    _oci_layout_settings = attr.label(
        default = Label("//img/private/settings:oci_layout"),
        providers = [OCILayoutSettingsInfo],
    ),
)

def normalize_variant(architecture, variant):
    """Returns 'v8' when architecture is 'arm64' and variant is empty, otherwise returns variant."""
    if architecture == "arm64" and variant == "":
        return "v8"
    return variant

def parse_platform_spec(platform_str):
    """Parses a platform specification string into a validated tuple.

    Args:
        platform_str: Platform in 'os/architecture' or 'os/architecture/variant' format.

    Returns:
        Tuple of (os, architecture, normalized_variant).
    """
    parsed = _parse_matching_spec(platform_str)
    if parsed.os not in SUPPORTED_OSES:
        fail("Unsupported OS '{}' in platform '{}'. Expected one of: {}".format(parsed.os, platform_str, SUPPORTED_OSES))
    if parsed.architecture not in SUPPORTED_ARCHITECTURES:
        fail("Unsupported architecture '{}' in platform '{}'. Expected one of: {}".format(parsed.architecture, platform_str, SUPPORTED_ARCHITECTURES))
    return (parsed.os, parsed.architecture, normalize_variant(parsed.architecture, parsed.variant))

def validate_and_resolve_compression(layer_media_types):
    """Validates layer media types and resolves a shared compression algorithm name.

    Args:
        layer_media_types: Ordered list of OCI layer media types.

    Returns:
        Compression algorithm name ('gzip', 'zstd', or 'none'), or None when compressed types are mixed.
    """
    if len(layer_media_types) == 0:
        fail("At least one layer media type must be specified.")
    compressed = {}
    for mt in layer_media_types:
        if mt not in LAYER_TYPES:
            fail("Unsupported layer media type: {}".format(mt))
        alg = MEDIA_TYPE_TO_COMPRESSION[mt]
        if alg != "none":
            compressed[alg] = True
    if len(compressed) == 1:
        return list(compressed.keys())[0]
    if len(compressed) == 0:
        return "none"
    return None

def declare_manifest_outputs(ctx, layer_media_types, *, suffix = "", include_digest = True):
    """Declares output files for a converted or flattened single-platform manifest.

    Args:
        ctx: Rule context.
        layer_media_types: Ordered list of OCI layer media types.
        suffix: Optional suffix appended to the target name for multi-platform children.
        include_digest: Whether to declare a manifest digest file.

    Returns:
        Struct with manifest, config, descriptor, digest, blobs, metadatas, and all_files.
    """
    prefix = ctx.attr.name + suffix
    manifest = ctx.actions.declare_file(prefix + "_manifest.json")
    config = ctx.actions.declare_file(prefix + "_config.json")
    descriptor = ctx.actions.declare_file(prefix + "_descriptor.json")
    digest = ctx.actions.declare_file(prefix + "_digest") if include_digest else None
    blobs = [
        ctx.actions.declare_file("{}_layer_{}.{}".format(prefix, i, LAYER_EXTENSIONS[mt]))
        for i, mt in enumerate(layer_media_types)
    ]
    metadatas = [
        ctx.actions.declare_file("{}_meta_{}.json".format(prefix, i))
        for i in range(len(layer_media_types))
    ]
    all_files = [manifest, config, descriptor] + ([digest] if digest != None else []) + blobs + metadatas
    return struct(
        manifest = manifest,
        config = config,
        descriptor = descriptor,
        digest = digest,
        blobs = blobs,
        metadatas = metadatas,
        all_files = all_files,
    )

def make_layer_infos(ctx, name_prefix, layer_media_types, blobs, metadatas):
    """Constructs SingleLayerInfo providers with pre-declared mtree actions for each converted layer."""
    return [
        SingleLayerInfo(
            blob = blobs[i],
            generated_blob = None,
            estargz = False,
            zstd_chunked = False,
            media_type = layer_media_types[i],
            metadata = metadatas[i],
            compact_stream = None,
            layer_input_files = None,
            layer_input_files_cas = None,
            sources = [],
            mtree = build_layer_mtree(ctx, "{}_layer_{}".format(name_prefix, i), tar_blob = blobs[i]),
            ztoc = None,
        )
        for i in range(len(layer_media_types))
    ]

def build_manifest_info(ctx, outs, layer_infos, *, os, architecture, variant, structured_config = None, suffix = ""):
    """Constructs an ImageManifestInfo provider with sparse OCI layout and optional image mtree."""
    prefix = ctx.attr.name + suffix
    sparse_layout = build_sparse_oci_layout_for_manifest(
        ctx,
        outs.manifest,
        outs.config,
        layer_infos,
        suffix = suffix,
    )
    return ImageManifestInfo(
        descriptor = outs.descriptor,
        manifest = outs.manifest,
        config = outs.config,
        structured_config = structured_config if structured_config != None else {"architecture": architecture, "os": os},
        architecture = architecture,
        os = os,
        variant = variant,
        layers = layer_infos,
        mtree = image_mtree_or_none(ctx, prefix, layer_infos),
        sparse_oci_layout = sparse_layout,
        soci_manifest = None,
        soci_config = None,
        soci_descriptor = None,
        soci_ztocs = [],
    )

def _build_oci_layout(ctx, format, *, manifest_info = None, index_out = None, manifests = None, mnemonic = "OCILayout"):
    output = ctx.actions.declare_directory(ctx.label.name + "_oci_layout") if format == "directory" else ctx.actions.declare_file(ctx.label.name + "_oci_layout.tar")
    args = ctx.actions.args()
    args.add("oci-layout")
    args.add("--format", format)
    if ctx.attr._oci_layout_settings[OCILayoutSettingsInfo].allow_shallow_oci_layout:
        args.add("--allow-missing-blobs")

    inputs = []
    if manifest_info != None:
        args.add("--manifest", manifest_info.manifest)
        args.add("--config", manifest_info.config)
        inputs.extend([manifest_info.manifest, manifest_info.config])
        all_layers = manifest_info.layers
    else:
        args.add("--index", index_out)
        inputs.append(index_out)
        all_layers = []
        for m in manifests:
            args.add("--manifest-path", m.manifest)
            args.add("--config-path", m.config)
            inputs.extend([m.manifest, m.config])
            all_layers.extend(m.layers)

    for layer in all_layers:
        blob = blob_for_layout(ctx, layer, format)
        if blob != None:
            args.add_joined("--layer", [layer.metadata, blob], join_with = "=")
            inputs.extend([layer.metadata, blob])

    run_oci_layout_action(
        ctx,
        format = format,
        output = output,
        args = args,
        inputs = inputs,
        mnemonic = mnemonic,
    )
    return output

def manifest_providers(ctx, manifest_info, digest_file, mnemonic = "OCILayout"):
    """Builds the standard rule providers for a single-platform conversion rule.

    Args:
        ctx: Rule context.
        manifest_info: Constructed ImageManifestInfo provider.
        digest_file: File containing the manifest digest.
        mnemonic: Action mnemonic for OCI layout generation.

    Returns:
        List containing DefaultInfo, OutputGroupInfo, and ImageManifestInfo.
    """
    groups = dict(
        descriptor = depset([manifest_info.descriptor]),
        digest = depset([digest_file]),
        root_blob = depset([manifest_info.manifest]),
        oci_layout = depset([_build_oci_layout(ctx, "directory", manifest_info = manifest_info, mnemonic = mnemonic)]),
        oci_tarball = depset([_build_oci_layout(ctx, "tar", manifest_info = manifest_info, mnemonic = mnemonic)]),
        sparse_oci_layout = depset([manifest_info.sparse_oci_layout]),
    )
    if manifest_info.mtree != None:
        groups["mtree"] = depset([manifest_info.mtree])
    return [
        DefaultInfo(files = depset([manifest_info.manifest, manifest_info.config])),
        OutputGroupInfo(**groups),
        manifest_info,
    ]

def index_providers(ctx, index_out, descriptor_out, digest_out, manifests, mnemonic = "OCIIndexLayout"):
    """Returns DefaultInfo, OutputGroupInfo, and ImageIndexInfo for a multi-platform conversion rule."""
    sparse_layout = build_sparse_oci_layout_for_index(ctx, index_out, manifests)
    return [
        DefaultInfo(files = depset([index_out])),
        OutputGroupInfo(
            descriptor = depset([descriptor_out]),
            digest = depset([digest_out]),
            root_blob = depset([index_out]),
            oci_layout = depset([_build_oci_layout(ctx, "directory", index_out = index_out, manifests = manifests, mnemonic = mnemonic)]),
            oci_tarball = depset([_build_oci_layout(ctx, "tar", index_out = index_out, manifests = manifests, mnemonic = mnemonic)]),
            sparse_oci_layout = depset([sparse_layout]),
        ),
        ImageIndexInfo(
            descriptor = descriptor_out,
            index = index_out,
            manifests = manifests,
            sparse_oci_layout = sparse_layout,
        ),
    ]
