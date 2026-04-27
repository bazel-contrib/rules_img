"""Compression testing utilities for rules_img.

This module provides functions and rules for testing various compression
configurations and combinations for container image layers.
"""

load("@bazel_skylib//rules:build_test.bzl", "build_test")
load("@rules_img//img:providers.bzl", "SingleLayerInfo")

# Predefined test combinations for compression parameter testing.
# Each tuple contains: (name_suffix, layer, mode, jobs, level, algo, estargz)
_combinations = [
    ("default", "large_1GB", "fastbuild", "auto", "auto", "gzip", "disabled"),
    ("estargz", "large_1GB", "fastbuild", "1", "9", "gzip", "enabled"),
    ("zstd", "large_1GB", "fastbuild", "1", "9", "zstd", "disabled"),
    ("opt", "large_1GB", "opt", "auto", "auto", "gzip", "disabled"),
    ("smallest", "large_1GB", "fastbuild", "1", "9", "gzip", "disabled"),
    ("nproc", "large_1GB", "fastbuild", "nproc", "auto", "gzip", "disabled"),
    ("jobs_2", "large_1GB", "fastbuild", "2", "auto", "gzip", "disabled"),
    ("jobs_4", "large_1GB", "fastbuild", "4", "auto", "gzip", "disabled"),
    ("jobs_8", "large_1GB", "fastbuild", "8", "auto", "gzip", "disabled"),
    ("jobs_16", "large_1GB", "fastbuild", "16", "auto", "gzip", "disabled"),
    ("jobs_32", "large_1GB", "fastbuild", "32", "auto", "gzip", "disabled"),
    ("jobs_64", "large_1GB", "fastbuild", "64", "auto", "gzip", "disabled"),
    ("jobs_128", "large_1GB", "fastbuild", "128", "auto", "gzip", "disabled"),
    ("level_0", "large_1GB", "fastbuild", "auto", "0", "gzip", "disabled"),
    ("level_1", "large_1GB", "fastbuild", "auto", "1", "gzip", "disabled"),
    ("level_2", "large_1GB", "fastbuild", "auto", "2", "gzip", "disabled"),
    ("level_3", "large_1GB", "fastbuild", "auto", "3", "gzip", "disabled"),
    ("level_4", "large_1GB", "fastbuild", "auto", "4", "gzip", "disabled"),
    ("level_5", "large_1GB", "fastbuild", "auto", "5", "gzip", "disabled"),
    ("level_6", "large_1GB", "fastbuild", "auto", "6", "gzip", "disabled"),
    ("level_7", "large_1GB", "fastbuild", "auto", "7", "gzip", "disabled"),
    ("level_8", "large_1GB", "fastbuild", "auto", "8", "gzip", "disabled"),
    ("level_9", "large_1GB", "fastbuild", "auto", "9", "gzip", "disabled"),
]

def layer_combinations(name = ""):
    """Creates test targets for various compression parameter combinations.

    Generates tuned_layer targets for testing different compression settings
    including jobs, levels, algorithms, and estargz configurations.

    Args:
        name: Optional prefix for the generated target names.
    """
    for (name_suffix, layer, mode, jobs, level, algo, estargz) in _combinations:
        tuned_layer(
            name = name + name_suffix,
            layer = ":" + layer,
            compilation_mode = mode,
            compression_jobs = jobs,
            compression_level = level,
            compress = algo,
            estargz = estargz == "enabled",
            tags = [
                "manual",
                "no-cache",
                "exclusive",
            ],
        )

    build_test(
        name = "test_" + name + "combinations",
        targets = [":" + name + suffix for (suffix, _, _, _, _, _, _) in _combinations],
        tags = [
            "manual",
            "no-cache",
            "exclusive",
        ],
    )

def _compression_setting_transition_impl(_settings, attr):
    """Implementation function for compression setting transitions.

    Translates rule attributes into configuration settings for compression
    parameters like jobs, level, algorithm, and estargz mode.

    Args:
        _settings: Current build settings (unused).
        attr: Rule attributes containing compression configuration.

    Returns:
        Dictionary mapping configuration keys to their new values.
    """
    return {
        "//command_line_option:compilation_mode": attr.compilation_mode or "fastbuild",
        "@rules_img//img/settings:compression_jobs": attr.compression_jobs or "auto",
        "@rules_img//img/settings:compression_level": attr.compression_level or "auto",
        "@rules_img//img/settings:compress": attr.compress,
        "@rules_img//img/settings:estargz": "enabled" if attr.estargz else "disabled",
    }

compression_setting_transition = transition(
    implementation = _compression_setting_transition_impl,
    inputs = [],
    outputs = [
        "//command_line_option:compilation_mode",
        "@rules_img//img/settings:compression_jobs",
        "@rules_img//img/settings:compression_level",
        "@rules_img//img/settings:compress",
        "@rules_img//img/settings:estargz",
    ],
)

def _tuned_layer_impl(ctx):
    """Implementation function for the tuned_layer rule.

    Forwards the layer blob and metadata from the input layer while applying
    the configured compression settings via the transition.

    Args:
        ctx: Rule context containing attributes and dependencies.

    Returns:
        List of providers including DefaultInfo, OutputGroupInfo, and SingleLayerInfo.
    """
    layer_info = ctx.attr.layer[SingleLayerInfo]
    return [
        DefaultInfo(files = depset([layer_info.blob])),
        OutputGroupInfo(
            layer = depset([layer_info.blob]),
            metadata = depset([layer_info.metadata]),
        ),
        layer_info,
    ]

tuned_layer = rule(
    doc = """A rule that applies specific compression settings to an image layer.

    This rule acts as a wrapper around an image_layer target, allowing you to
    override compression parameters like algorithm, level, and parallelism
    settings via a configuration transition. It's primarily used for testing
    different compression configurations.
    """,
    implementation = _tuned_layer_impl,
    attrs = {
        "layer": attr.label(
            mandatory = True,
            doc = "The image_layer target to apply the tuned settings to.",
            providers = [SingleLayerInfo],
        ),
        "compilation_mode": attr.string(
            default = "fastbuild",
            doc = "Sets the compilation mode for determining compression level defaults. One of 'fastbuild', 'dbg', or 'opt'.",
            values = [
                "fastbuild",
                "dbg",
                "opt",
            ],
        ),
        "compression_jobs": attr.string(
            default = "auto",
            doc = "Overrides the number of jobs used for compression. 'auto' uses compilation mode, 'nproc' uses number of CPUs.",
        ),
        "compression_level": attr.string(
            default = "auto",
            doc = "Overrides the compression level. For gzip, this is between 0 (no compression) and 9 (best compression). 'auto' uses compilation mode defaults (-1 for default, 1 for fastbuild, 9 for opt).",
        ),
        "compress": attr.string(
            default = "gzip",
            values = ["gzip", "zstd"],
            doc = "Compression algorithm to use for the layer.",
        ),
        "estargz": attr.bool(
            default = False,
            doc = "Whether to create an estargz layer (gzip with special TOC for lazy loading).",
        ),
    },
    cfg = compression_setting_transition,
)
