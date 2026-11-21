# Copied verbatim (except load paths) from
# https://github.com/bazel-contrib/supply-chain/blob/c18a7daa7a4ed5e6d523a19f070c0cba19118419/tools/gather_metadata/providers.bzl
# (Apache-2.0). Keep the shape identical: rules_img switches to
# @supply_chain_tools//gather_metadata once it is released to the BCR.

"""Providers for transitively gathering all license and package_info targets.

Warning: This is private to the aspect that walks the tree. The API is subject
to change at any release.
"""

TargetWithMetadataInfo = provider(
    doc = """A target and the assocated metadata for it.""",
    fields = {
        "target": "Label: A target which will be associated with some metadata.",
        "metadata": "depset(): [list] of my direct collected leaf attribute providers",
        "direct_deps": "list<Label>: Direct dependencies of this target (for graph edges)",
    },
)

TransitiveMetadataInfo = provider(
    doc = """The transitive set of TargetWithMetadataInfo objects.""",
    fields = {
        "transitive": "depset(): transitive collection of TWMI",
        "top_level_target": "Label: The top level target label we are examining.",
    },
)

null_transitive_metadata_info = TransitiveMetadataInfo(
    transitive = depset(),
    top_level_target = None,
)
