"""Builds images with another automatic referrers toolchain than the registered one."""

def _toolchain_transition_impl(_settings, attr):
    return {"//command_line_option:extra_toolchains": [attr.toolchain]}

_toolchain_transition = transition(
    implementation = _toolchain_transition_impl,
    inputs = [],
    outputs = ["//command_line_option:extra_toolchains"],
)

def _sboms_impl(ctx):
    return [DefaultInfo(files = depset(transitive = [image[OutputGroupInfo].sbom for image in ctx.attr.images]))]

sboms_with_toolchain = rule(
    implementation = _sboms_impl,
    doc = "The SBOMs of `images`, built with `toolchain` (a toolchain label) first in line.",
    attrs = {
        "images": attr.label_list(cfg = _toolchain_transition),
        "toolchain": attr.string(mandatory = True),
    },
)
