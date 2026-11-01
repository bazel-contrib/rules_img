"""Custom rules that extend rules_img functionality for testing purposes."""

load("@rules_img//img:image.bzl", "image_index", "image_manifest")
load("@rules_img//img:layer.bzl", "image_layer")

MyInfo = provider(
    doc = "A custom provider for demonstration purposes.",
    fields = {
        "comment": "A user-defined comment.",
    },
)

def _noop_transition_impl(_settings, _attr):
    return {}

noop_transition = transition(
    implementation = _noop_transition_impl,
    inputs = [],
    outputs = [],
)

# This is necessary to work around the error message:
# "Error in rule: Unused function-based split transition allowlist: //extend:defs.bzl NORMAL"
# It occurs when a parent rule has a function-based transition that the extending rule does not use itself.
# See https://cs.opensource.google/bazel/bazel/+/master:src/main/java/com/google/devtools/build/lib/analysis/starlark/StarlarkRuleClassFunctions.java;l=1209-1214;drc=bbf65aea398c490db92a77d7ec077d9a623ce3ff
_MANDATORY_TRANSITION = attr.label(cfg = noop_transition)

def _customized_image_layer_impl(ctx):
    target = ctx.super()
    return [MyInfo(comment = ctx.attr.comment)] + target

customized_image_layer = rule(
    implementation = _customized_image_layer_impl,
    parent = image_layer,
    attrs = {
        "comment": attr.string(mandatory = True, doc = "A user-defined comment."),
        "_mandatory_transition": _MANDATORY_TRANSITION,
    },
)

def _customized_image_manifest_impl(ctx):
    target = ctx.super()
    return [MyInfo(comment = ctx.attr.comment)] + target

customized_image_manifest = rule(
    implementation = _customized_image_manifest_impl,
    parent = image_manifest,
    attrs = {
        "comment": attr.string(mandatory = True, doc = "A user-defined comment."),
        "_mandatory_transition": _MANDATORY_TRANSITION,
    },
)

def _customized_image_index_impl(ctx):
    target = ctx.super()
    return [MyInfo(comment = ctx.attr.comment)] + target

customized_image_index = rule(
    implementation = _customized_image_index_impl,
    parent = image_index,
    attrs = {
        "comment": attr.string(mandatory = True, doc = "A user-defined comment."),
        "_mandatory_transition": _MANDATORY_TRANSITION,
    },
)
