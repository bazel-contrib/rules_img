"""Aspects that collect the inputs of automatic referrers (SBOMs).

Both are always attached by the image rules, but return at once unless an
automatic referrers toolchain is registered: an aspect's propagation is decided
before toolchains are resolved, so it can't be skipped altogether.
"""

load("@bazel_features//:features.bzl", "bazel_features")
load("@package_metadata//:defs.bzl", "PackageAttributeInfo", "PackageMetadataInfo")
load("@package_metadata//licenses/providers:license_kind_info.bzl", "LicenseKindInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/common:layer_helper.bzl", "allow_tar_files", "extension_to_compression")
load("//img/private/providers:automatic_referrers_info.bzl", "BaseSbomInfo", "ImageSbomInfo", "LayerScanInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:layers_info.bzl", "LayersInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")
load("//img/private/providers:pulled_referrers_info.bzl", "PulledReferrersInfo")
load("//img/private/supply_chain_tools/gather_metadata:core.bzl", "gather_metadata_info_common", "should_traverse")
load("//img/private/supply_chain_tools/gather_metadata:providers.bzl", "TransitiveMetadataInfo", "null_transitive_metadata_info")

AUTOMATIC_REFERRERS_TOOLCHAIN = str(Label("//img:automatic_referrers_toolchain_type"))

OPTIONAL_AUTOMATIC_REFERRERS_TOOLCHAIN = config_common.toolchain_type(AUTOMATIC_REFERRERS_TOOLCHAIN, mandatory = False)

def automatic_referrers_toolchain(ctx):
    """Return the AutomaticReferrersToolchainInfo of the registered toolchain, or None.

    Args:
        ctx: Rule or aspect context that declares OPTIONAL_AUTOMATIC_REFERRERS_TOOLCHAIN.

    Returns:
        AutomaticReferrersToolchainInfo or None.
    """
    toolchain = ctx.toolchains[AUTOMATIC_REFERRERS_TOOLCHAIN]
    if toolchain == None:
        return None
    return toolchain.automatic_referrers

def _metadata_aspect_impl(target, ctx):
    if ctx.toolchains[AUTOMATIC_REFERRERS_TOOLCHAIN] == None:
        return [null_transitive_metadata_info]

    # The implementation of @supply_chain_tools//gather_metadata:gather_metadata.bzl%gather_metadata_info.
    return gather_metadata_info_common(
        target,
        ctx,
        want_providers = [PackageAttributeInfo, PackageMetadataInfo, LicenseKindInfo],
        provider_factory = TransitiveMetadataInfo,
        null_provider_instance = null_transitive_metadata_info,
        filter_func = should_traverse,
        enable_intermediate_pruning = False,
    )

def _metadata_propagation(ctx):
    # Only follow the edges gather_metadata_info_common would traverse anyway, so
    # the aspect isn't evaluated for the targets behind them at all.
    kind = struct(rule = struct(kind = ctx.rule.qualified_kind.rule_name))
    return [name for name in dir(ctx.rule.attr) if should_traverse(kind, name)]

metadata_aspect = aspect(
    doc = """Collects package metadata of an image's layers into TransitiveMetadataInfo.

Like `@supply_chain_tools//gather_metadata:gather_metadata.bzl%gather_metadata_info`,
but a no-op without an automatic referrers toolchain.""",
    implementation = _metadata_aspect_impl,
    attr_aspects = _metadata_propagation if bazel_features.rules.aspect_propagation_context else ["*"],
    provides = [TransitiveMetadataInfo],
    apply_to_generating_rules = True,
    toolchains = [OPTIONAL_AUTOMATIC_REFERRERS_TOOLCHAIN],
)

_NO_BASE_SBOM = BaseSbomInfo(image_sbom = None, referrers = None, index_info = None, pull_info = None)

def _base_sbom_aspect_impl(target, ctx):
    if ctx.toolchains[AUTOMATIC_REFERRERS_TOOLCHAIN] == None:
        return [_NO_BASE_SBOM]

    # A base built in this build with an SBOM: it already describes its own base.
    if ImageSbomInfo in target:
        return [BaseSbomInfo(
            image_sbom = target[ImageSbomInfo],
            referrers = None,
            index_info = target[ImageIndexInfo] if ImageIndexInfo in target else None,
            pull_info = None,
        )]

    # A pulled base: its SBOMs are among its referrers.
    if PulledReferrersInfo in target:
        return [BaseSbomInfo(
            image_sbom = None,
            referrers = target[PulledReferrersInfo],
            index_info = target[ImageIndexInfo] if ImageIndexInfo in target else None,
            pull_info = target[PullInfo] if PullInfo in target else None,
        )]

    # Anything else (a base built without an SBOM, ...): keep walking up its base.
    base = getattr(ctx.rule.attr, "base", None)
    if type(base) == "Target" and BaseSbomInfo in base:
        return [base[BaseSbomInfo]]
    return [_NO_BASE_SBOM]

base_sbom_aspect = aspect(
    doc = "Walks up `base` to the nearest image with an SBOM: a built image's ImageSbomInfo, or a pulled image's referrers.",
    implementation = _base_sbom_aspect_impl,
    attr_aspects = ["base"],
    provides = [BaseSbomInfo],
    toolchains = [OPTIONAL_AUTOMATIC_REFERRERS_TOOLCHAIN],
)

_NO_LAYER_SCANS = LayerScanInfo(scans = depset())

_OPTIONAL_IMG_TOOLCHAIN = config_common.toolchain_type(TOOLCHAIN, mandatory = False)

# The attributes along which image rules reach the layers of an image: an image's own layers,
# its base image, and the manifests of an image index used as a base.
_LAYER_SCAN_ATTRS = ["base", "layers", "manifests"]

def _dep_targets(rule_attr, name):
    value = getattr(rule_attr, name, None)
    if type(value) == "Target":
        return [value]
    if type(value) == "list":
        return [dep for dep in value if type(dep) == "Target"]
    return []

def _plain_tar_media_type(file):
    for extension in allow_tar_files:
        if file.basename.endswith(extension):
            compression = extension_to_compression[file.extension]
            media_type = "application/vnd.oci.image.layer.v1.tar"
            return media_type if compression == "none" else media_type + "+" + compression
    return None

def layer_contents(ctx, *, media_type, blob, compact_stream = None, cas = None):
    """How to read a layer's files as an uncompressed tar, or None if they aren't available.

    Args:
        ctx: Rule or aspect context that declares the img toolchain (possibly optional).
        media_type: The layer's media type.
        blob: The layer blob File, or None.
        compact_stream: The layer's compact stream File, or None.
        cas: The tree artifact with the content the compact stream refers to, or None.

    Returns:
        struct(executable, arguments, inputs), or None.
    """
    toolchain = ctx.toolchains[TOOLCHAIN]
    if toolchain == None:
        return None
    arguments = ["layer-tar", "--media-type", media_type or ""]
    if blob != None:
        arguments.extend(["--blob", blob.path])
        inputs = [blob]
    elif compact_stream != None and cas != None:
        arguments.extend(["--compact-stream", compact_stream.path, "--cas-dir", cas.path])
        inputs = [compact_stream, cas]
    else:
        return None
    arguments.extend(["--output", "-"])
    return struct(
        executable = toolchain.imgtoolchaininfo.tool_exe,
        arguments = arguments,
        inputs = depset(inputs),
    )

def _scan_request(ctx, *, name, origin, media_type, metadata, layer, contents):
    return struct(
        label = ctx.label,
        name = name,
        origin = origin,
        media_type = media_type,
        metadata = metadata,
        layer = layer,
        contents = contents,
    )

def _layer_scan_aspect_impl(target, ctx):
    toolchain = automatic_referrers_toolchain(ctx)
    scan_layer = getattr(toolchain, "scan_layer", None) if toolchain != None else None
    if scan_layer == None:
        return [_NO_LAYER_SCANS]

    deps = [dep for name in _LAYER_SCAN_ATTRS for dep in _dep_targets(ctx.rule.attr, name)]
    transitive = [dep[LayerScanInfo].scans for dep in deps if LayerScanInfo in dep]

    # The layers this target provides. Those of an image built on top of others (an
    # image_manifest, an image_index) come from what the aspect visited before, along
    # _LAYER_SCAN_ATTRS, and are scanned there: only a layer this target made itself is
    # scanned here. A pulled image's are all its own.
    layers = []
    if LayersInfo in target:
        layers.extend(target[LayersInfo].layers)
    if ImageManifestInfo in target:
        layers.extend(target[ImageManifestInfo].layers)
    if ImageIndexInfo in target:
        for manifest in target[ImageIndexInfo].manifests:
            layers.extend(manifest.layers)
    plain_tars = []
    if not layers and not deps and DefaultInfo in target:
        # A plain tar file (from rules_pkg, ...) used as a layer.
        plain_tars = [f for f in target[DefaultInfo].files.to_list() if _plain_tar_media_type(f) != None]
    if not layers and not plain_tars:
        return [LayerScanInfo(scans = depset(transitive = transitive))]

    covered = {}
    for scans in transitive:
        for scan in scans.to_list():
            for key in scan.keys:
                covered[key] = True

    # An image built on top of a pulled one forwards its PullInfo too, but such an image is
    # reached along _LAYER_SCAN_ATTRS (and it has the deps to show for it).
    pulled = ctx.rule.kind == "image_import" or (PullInfo in target and not deps)
    direct = []
    for (position, layer) in enumerate(layers):
        keys = tuple([f for f in [layer.metadata, layer.blob] if f != None])
        if not keys or [key for key in keys if key in covered]:
            continue
        for key in keys:
            covered[key] = True
        request = _scan_request(
            ctx,
            name = "{}_layer_scan_{}".format(target.label.name, position),
            origin = "pulled" if pulled or layer.sources else "built",
            media_type = layer.media_type,
            metadata = layer.metadata,
            layer = layer,
            contents = layer_contents(
                ctx,
                media_type = layer.media_type,
                blob = layer.blob,
                compact_stream = layer.compact_stream,
                cas = layer.layer_input_files_cas,
            ),
        )
        direct.append(struct(keys = keys, origin = request.origin, scan = scan_layer(ctx, toolchain, request)))
    for (position, tar) in enumerate(plain_tars):
        if tar in covered:
            continue
        covered[tar] = True
        media_type = _plain_tar_media_type(tar)
        request = _scan_request(
            ctx,
            name = "{}_layer_scan_tar_{}".format(target.label.name, position),
            origin = "built",
            media_type = media_type,
            metadata = None,
            layer = None,
            contents = layer_contents(ctx, media_type = media_type, blob = tar),
        )
        direct.append(struct(keys = (tar,), origin = "built", scan = scan_layer(ctx, toolchain, request)))
    return [LayerScanInfo(scans = depset(direct, transitive = transitive))]

layer_scan_aspect = aspect(
    doc = """Scans the layers of an image, and of its base images, with the automatic referrers toolchain's `scan_layer`.

Every layer is scanned at the target that provides it (a layer rule, a pulled image), so a
layer shared by many images is scanned once. A no-op unless the toolchain scans layers.""",
    implementation = _layer_scan_aspect_impl,
    attr_aspects = _LAYER_SCAN_ATTRS,
    provides = [LayerScanInfo],
    toolchains = [OPTIONAL_AUTOMATIC_REFERRERS_TOOLCHAIN, _OPTIONAL_IMG_TOOLCHAIN],
)

def layer_scans(deps):
    """Index the layer scans of the given targets (deps carrying the layer scan aspect).

    Args:
        deps: Targets that may carry LayerScanInfo.

    Returns:
        Dict mapping each File that identifies a layer to struct(origin, scan).
    """
    index = {}
    for dep in deps:
        if dep == None or LayerScanInfo not in dep:
            continue
        for scan in dep[LayerScanInfo].scans.to_list():
            for key in scan.keys:
                if key not in index:
                    index[key] = scan
    return index
