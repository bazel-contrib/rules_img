"""Helpers for the tests of disk images."""

load("@rules_img//img:providers.bzl", "DiskImageInfo", "LayersInfo")

def _compact_layers_impl(_settings, _attr):
    return {
        str(Label("//img/settings:experimental_compact_layers")): "enabled",
    }

_compact_layers = transition(
    implementation = _compact_layers_impl,
    inputs = [],
    outputs = [str(Label("//img/settings:experimental_compact_layers"))],
)

def _with_compact_layers_impl(ctx):
    dep = ctx.attr.dep
    if type(dep) == "list":
        dep = dep[0]
    providers = [DefaultInfo(files = dep[DefaultInfo].files)]
    for provider in [OutputGroupInfo, DiskImageInfo, LayersInfo]:
        if provider in dep:
            providers.append(dep[provider])
    return providers

with_compact_layers = rule(
    implementation = _with_compact_layers_impl,
    doc = "Builds a target with experimental_compact_layers enabled and forwards its files, output groups, DiskImageInfo and LayersInfo.",
    attrs = {
        "dep": attr.label(mandatory = True, cfg = _compact_layers),
        "_allowlist_function_transition": attr.label(
            default = "@bazel_tools//tools/allowlists/function_transition_allowlist",
        ),
    },
)
