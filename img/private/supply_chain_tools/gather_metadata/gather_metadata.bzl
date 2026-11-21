# Copied verbatim (except load paths) from
# https://github.com/bazel-contrib/supply-chain/blob/c18a7daa7a4ed5e6d523a19f070c0cba19118419/tools/gather_metadata/gather_metadata.bzl
# (Apache-2.0). Keep the shape identical: rules_img switches to
# @supply_chain_tools//gather_metadata once it is released to the BCR.

"""Rules and macros for collecting metadata providers."""

load(
    "@package_metadata//:defs.bzl",
    "PackageAttributeInfo",
    "PackageMetadataInfo",
)
load(
    "@package_metadata//licenses/providers:license_kind_info.bzl",
    "LicenseKindInfo",
)
load(
    ":core.bzl",
    "gather_metadata_info_common",
    "should_traverse",
)
load(":providers.bzl", "TransitiveMetadataInfo", "null_transitive_metadata_info")
load(
    ":serialization.bzl",
    "metadata_info_to_json",
    "write_metadata_info",
)

def _gather_metadata_info_impl(target, ctx):
    return gather_metadata_info_common(
        target,
        ctx,
        want_providers = [PackageAttributeInfo, PackageMetadataInfo, LicenseKindInfo],
        provider_factory = TransitiveMetadataInfo,
        null_provider_instance = null_transitive_metadata_info,
        filter_func = should_traverse,
        enable_intermediate_pruning = False,
    )

gather_metadata_info = aspect(
    doc = """Collects metadata providers into a single TransitiveMetadataInfo provider.""",
    implementation = _gather_metadata_info_impl,
    attr_aspects = ["*"],
    provides = [TransitiveMetadataInfo],
    apply_to_generating_rules = True,
)

def _write_metadata_info_impl(target, ctx):
    """Write transitive metadata info into a JSON file.

    Args:
      target: The target of the aspect.
      ctx: The aspect evaluation context.

    Returns:
      OutputGroupInfo with metadata files
    """

    if not TransitiveMetadataInfo in target:
        return [OutputGroupInfo(licenses = depset())]
    info = target[TransitiveMetadataInfo]

    # Write the output file for the target
    name = "%s_metadata_info.json" % ctx.label.name
    json_strings = metadata_info_to_json(info)
    content = json_strings[0] if json_strings else "{}"
    out = ctx.actions.declare_file(name)
    ctx.actions.write(
        output = out,
        content = content,
    )

    return [OutputGroupInfo(metadata = depset([out]))]

gather_metadata_info_and_write = aspect(
    doc = """Collects TransitiveMetadataInfo providers and writes JSON representation to a file.

    Usage:
      bazel build //some:target \
          --aspects=@supply_chain_tools//gather_metadata:gather_metadata.bzl%gather_metadata_info_and_write \
          --output_groups=metadata

    Output format:
      Graph-only JSON with the structure:
      {
        "schema_version": "1.0",
        "root_target": "//some:target",
        "nodes": [
          {
            "label": "//some:target",
            "metadata_file": "bazel-bin/some/target.package-metadata.json"
          }
        ],
        "edges": [
          {
            "from": "//some:target",
            "to": "//dep:target",
            "type": "depends_on"
          }
        ]
      }
    """,
    implementation = _write_metadata_info_impl,
    attr_aspects = ["*"],
    provides = [OutputGroupInfo],
    requires = [gather_metadata_info],
    apply_to_generating_rules = True,
)
