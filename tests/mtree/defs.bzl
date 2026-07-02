"""Golden tests for the layer `mtree` output group.

`mtree_golden_test` builds a layer's `mtree` output group with the default (tar)
layout, both with compact layers disabled (a materialized tar blob) and enabled
(a compact stream), and diffs each against a single golden -- proving the mtree
is identical across the two modes and matches the checked-in golden.

`mtree_changeset_golden_test` builds a layer's `mtree` with the layer layout
pinned to "oci_layer_filesystem_applied_changeset" (and every field enabled) and
diffs it against a golden.

Both reuse `_extract_mtree`, which builds the `layer` under a transition that
pins the //img/settings:experimental_compact_layers, :mtree_layer_layout, and
:mtree_options build settings, then exposes the single `mtree` output-group file.
"""

load("@bazel_skylib//rules:diff_test.bzl", "diff_test")

_COMPACT_SETTING = "@rules_img//img/settings:experimental_compact_layers"
_LAYOUT_SETTING = "@rules_img//img/settings:mtree_layer_layout"
_OPTIONS_SETTING = "@rules_img//img/settings:mtree_options"

# The default field set (matches //img/settings:mtree_options' default) plus the
# full set that additionally emits extended attributes.
DEFAULT_OPTIONS = "type,size,mode,uid,uname,gid,gname,sha256,time,link,nlink"
ALL_OPTIONS = DEFAULT_OPTIONS + ",xattr"

def _pin_mtree_settings_impl(_settings, attr):
    return {
        _COMPACT_SETTING: attr.compact,
        _LAYOUT_SETTING: attr.layout,
        _OPTIONS_SETTING: attr.options,
    }

_pin_mtree_settings = transition(
    implementation = _pin_mtree_settings_impl,
    inputs = [],
    outputs = [_COMPACT_SETTING, _LAYOUT_SETTING, _OPTIONS_SETTING],
)

def _extract_mtree_impl(ctx):
    # A transition is attached to `layer`, so the attribute is a list of length 1.
    target = ctx.attr.layer[0]
    mtrees = target[OutputGroupInfo].mtree.to_list()
    if len(mtrees) != 1:
        fail("mtree golden test supports single-layer targets only; {} produced {} mtree files".format(
            target.label,
            len(mtrees),
        ))
    return [DefaultInfo(files = depset(mtrees))]

_extract_mtree = rule(
    implementation = _extract_mtree_impl,
    doc = "Builds `layer` with the mtree build settings pinned and exposes its single `mtree` output-group file.",
    attrs = {
        "layer": attr.label(
            mandatory = True,
            cfg = _pin_mtree_settings,
            doc = "A single-layer target providing an `mtree` output group.",
        ),
        "compact": attr.string(
            mandatory = True,
            values = ["disabled", "enabled"],
            doc = "Value to pin experimental_compact_layers to.",
        ),
        "layout": attr.string(
            mandatory = True,
            values = ["tar", "oci_layer_filesystem_applied_changeset"],
            doc = "Value to pin mtree_layer_layout to.",
        ),
        "options": attr.string(
            mandatory = True,
            doc = "Value to pin mtree_options to.",
        ),
    },
)

def mtree_golden_test(name, layer, golden):
    """Asserts a layer's tar-layout mtree matches a golden in both compact and non-compact modes.

    Args:
      name: Base name for the generated targets.
      layer: A single-layer target providing an `mtree` output group.
      golden: The golden file to diff against.
    """
    for compact in ("disabled", "enabled"):
        extract = "{}_{}_mtree".format(name, compact)
        _extract_mtree(
            name = extract,
            layer = layer,
            compact = compact,
            layout = "tar",
            options = DEFAULT_OPTIONS,
            testonly = True,
            tags = ["manual"],
        )
        diff_test(
            name = "{}_{}".format(name, compact),
            file1 = golden,
            file2 = ":" + extract,
            size = "small",
        )

def mtree_changeset_golden_test(name, layer, golden):
    """Asserts a layer's OCI applied-changeset mtree (all fields) matches a golden.

    Args:
      name: Base name for the generated targets.
      layer: A single-layer target providing an `mtree` output group.
      golden: The golden file to diff against.
    """
    extract = name + "_mtree"
    _extract_mtree(
        name = extract,
        layer = layer,
        compact = "disabled",
        layout = "oci_layer_filesystem_applied_changeset",
        options = ALL_OPTIONS,
        testonly = True,
        tags = ["manual"],
    )
    diff_test(
        name = name,
        file1 = golden,
        file2 = ":" + extract,
        size = "small",
    )
