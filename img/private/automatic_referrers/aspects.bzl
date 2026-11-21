"""Aspects that collect the inputs of automatic referrers (SBOMs).

Both are always attached by the image rules, but return at once unless an
automatic referrers toolchain is registered: an aspect's propagation is decided
before toolchains are resolved, so it can't be skipped altogether.
"""

load("@bazel_features//:features.bzl", "bazel_features")
load("@package_metadata//:defs.bzl", "PackageAttributeInfo", "PackageMetadataInfo")
load("@package_metadata//licenses/providers:license_kind_info.bzl", "LicenseKindInfo")
load("//img/private/providers:automatic_referrers_info.bzl", "BaseSbomInfo", "ImageSbomInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
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
