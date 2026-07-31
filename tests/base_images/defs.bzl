"""Test helpers for the base image rules.

The rules here build a `base_image_layer` under an explicitly pinned target
platform and diff its `mtree` output group against a golden. That covers both
what the content rules describe and the platform scoping: the same targets are
built for Linux and for Windows, and the Windows golden shows that the
Linux-only rules contributed nothing.

An mtree golden is used rather than an analysis test because it checks the thing
that actually matters -- the tar entries, their modes and their ownership -- and
does so after the Go tool has run, not just after analysis.
"""

load("@bazel_skylib//rules:diff_test.bzl", "diff_test")
load("@rules_img//img:providers.bzl", "LayersInfo")

def _pin_platform_impl(_settings, attr):
    return {"//command_line_option:platforms": str(attr.platform)}

_pin_platform = transition(
    implementation = _pin_platform_impl,
    inputs = [],
    outputs = ["//command_line_option:platforms"],
)

def _layer_mtree_impl(ctx):
    # A transition is attached to `layer`, so the attribute is a list of length 1.
    target = ctx.attr.layer[0]
    mtrees = target[OutputGroupInfo].mtree.to_list()
    if len(mtrees) != 1:
        fail("expected exactly one mtree file from {}, got {}".format(target.label, len(mtrees)))
    return [DefaultInfo(files = depset(mtrees))]

_layer_mtree = rule(
    implementation = _layer_mtree_impl,
    doc = "Builds a layer for a fixed target platform and exposes its mtree output group.",
    attrs = {
        "layer": attr.label(
            mandatory = True,
            cfg = _pin_platform,
            providers = [LayersInfo],
        ),
        "platform": attr.label(
            mandatory = True,
            doc = "Target platform to build the layer for.",
        ),
    },
)

def base_layer_mtree_test(*, name, layer, platform, golden):
    """Asserts that a base_image_layer built for a platform matches a golden mtree.

    Args:
        name: Name of the test target.
        layer: The `base_image_layer` to build.
        platform: Target platform to build it for.
        golden: The checked-in mtree file to compare against.
    """
    _layer_mtree(
        name = name + "_mtree",
        layer = layer,
        platform = platform,
        testonly = True,
    )
    diff_test(
        name = name,
        file1 = golden,
        file2 = name + "_mtree",
        # The mtree carries no host-specific data, but line endings differ on a
        # Windows checkout, which would make the diff spurious.
        target_compatible_with = select({
            "@platforms//os:windows": ["@platforms//:incompatible"],
            "//conditions:default": [],
        }),
    )
