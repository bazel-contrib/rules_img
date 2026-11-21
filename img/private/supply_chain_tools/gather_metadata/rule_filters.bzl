# Copied verbatim (except load paths) from
# https://github.com/bazel-contrib/supply-chain/blob/c18a7daa7a4ed5e6d523a19f070c0cba19118419/tools/gather_metadata/rule_filters.bzl
# (Apache-2.0). Keep the shape identical: rules_img switches to
# @supply_chain_tools//gather_metadata once it is released to the BCR.

"""Attribute exclusions for metadata gathering.

The format of this dictionary is:

  rule_name: [attr, attr, ...]

Only filters for rules that are part of the Bazel distribution should be added
to this file. Other filters should be added in user_filtered_rule_kinds.bzl

Attributes are either the explicit list of attributes to filter, or '_*' which
would ignore all attributes prefixed with a _.
"""

# Rule kinds with attributes the aspect currently needs to ignore
rule_to_excluded_attributes = {
    "*": ["linter", "_*"],
    "cc_binary": ["_*"],
    "cc_embed_data": ["_*"],
    "cc_grpc_library": ["_*", "current_cc_toolchain"],
    "cc_library": ["_*"],
    "cc_toolchain_alias": ["*"],
    "genrule": ["tools", "exec_tools", "toolchains"],
    "genyacc": ["_*"],
    "go_binary": ["_*"],
    "go_library": ["_*"],
    "go_wrap_cc": ["_*"],
    "java_binary": ["_*", "plugins", "exported_plugins"],
    "java_library": ["plugins", "exported_plugins"],
    "java_wrap_cc": ["_cc_toolchain", "swig_top"],
    "py_binary": ["_*"],
    "py_extension": ["_cc_toolchain"],
    "sh_binary": ["_bash_binary"],
    "_constant_gen": ["_generator"],
}
