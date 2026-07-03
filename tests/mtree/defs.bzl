"""Golden tests for the layer and image `mtree` output groups.

`mtree_golden_test` builds a layer's `mtree` output group with the default (tar)
layout, both with compact layers disabled (a materialized tar blob) and enabled
(a compact stream), and diffs each against a single golden -- proving the mtree
is identical across the two modes and matches the checked-in golden.

`mtree_changeset_golden_test` builds a layer's `mtree` with the layer layout
pinned to "oci_layer_filesystem_applied_changeset" (and every field enabled) and
diffs it against a golden.

`image_mtree_golden_test` builds an `image_manifest`'s `mtree` output group -- the
per-layer mtree files merged (in layer order) into one image-level mtree -- both
with compact layers disabled (materialized tar blobs) and enabled (compact
streams), and diffs each against a single golden, proving the merged image mtree
is identical across the two modes.

The two layer-level macros (`mtree_golden_test`, `mtree_changeset_golden_test`)
reuse `_extract_mtree`, which builds the `layer` under a transition pinning the
//img/settings:experimental_compact_layers, :mtree_layer_layout, and :mtree_options
build settings. The image-level macro (`image_mtree_golden_test`) uses
`_extract_image_mtree`, whose transition additionally pins :mtree_image_layout and
:mtree_path_prefix and operates on an `image_manifest` target. Both extractors
expose the single `mtree` output-group file.
"""

load("@bazel_skylib//rules:diff_test.bzl", "diff_test")

_COMPACT_SETTING = "@rules_img//img/settings:experimental_compact_layers"
_LAYOUT_SETTING = "@rules_img//img/settings:mtree_layer_layout"
_IMAGE_LAYOUT_SETTING = "@rules_img//img/settings:mtree_image_layout"
_OPTIONS_SETTING = "@rules_img//img/settings:mtree_options"
_PATH_PREFIX_SETTING = "@rules_img//img/settings:mtree_path_prefix"

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

def _pin_image_mtree_settings_impl(_settings, attr):
    return {
        _COMPACT_SETTING: attr.compact,
        _IMAGE_LAYOUT_SETTING: attr.image_layout,
        _LAYOUT_SETTING: attr.layer_layout,
        _OPTIONS_SETTING: attr.options,
        # Pin the path prefix too so the golden is hermetic against the ambient
        # //img/settings:mtree_path_prefix setting (it shapes every entry path).
        _PATH_PREFIX_SETTING: "./",
    }

_pin_image_mtree_settings = transition(
    implementation = _pin_image_mtree_settings_impl,
    inputs = [],
    outputs = [_COMPACT_SETTING, _IMAGE_LAYOUT_SETTING, _LAYOUT_SETTING, _OPTIONS_SETTING, _PATH_PREFIX_SETTING],
)

def _extract_image_mtree_impl(ctx):
    # A transition is attached to `image`, so the attribute is a list of length 1.
    target = ctx.attr.image[0]
    mtrees = target[OutputGroupInfo].mtree.to_list()
    if len(mtrees) != 1:
        fail("image mtree golden test expects exactly one merged mtree file; {} produced {}".format(
            target.label,
            len(mtrees),
        ))
    return [DefaultInfo(files = depset(mtrees))]

_extract_image_mtree = rule(
    implementation = _extract_image_mtree_impl,
    doc = "Builds `image` with the mtree build settings pinned and exposes its single merged `mtree` output-group file.",
    attrs = {
        "image": attr.label(
            mandatory = True,
            cfg = _pin_image_mtree_settings,
            doc = "An image_manifest target providing an `mtree` output group.",
        ),
        "compact": attr.string(
            mandatory = True,
            values = ["disabled", "enabled"],
            doc = "Value to pin experimental_compact_layers to.",
        ),
        "image_layout": attr.string(
            mandatory = True,
            values = ["tar", "oci_layer_filesystem_applied_changeset"],
            doc = "Value to pin mtree_image_layout to.",
        ),
        "layer_layout": attr.string(
            mandatory = True,
            values = ["tar", "oci_layer_filesystem_applied_changeset"],
            doc = "Value to pin mtree_layer_layout to (the per-layer mtree feeding the merge).",
        ),
        "options": attr.string(
            mandatory = True,
            doc = "Value to pin mtree_options to.",
        ),
    },
)

def image_mtree_golden_test(name, image, golden):
    """Asserts an image_manifest's merged mtree matches a golden in both compact and non-compact modes.

    The image `mtree` output group is built with the merged layout pinned to
    "oci_layer_filesystem_applied_changeset" and the per-layer layout pinned to
    "tar" (so per-layer whiteouts survive into the changeset merge). Building with
    compact layers both disabled and enabled proves the merged mtree is identical
    whether the layers are materialized tars or compact streams.

    Args:
      name: Base name for the generated targets.
      image: An image_manifest target providing an `mtree` output group.
      golden: The golden file to diff against.
    """
    for compact in ("disabled", "enabled"):
        extract = "{}_{}_mtree".format(name, compact)
        _extract_image_mtree(
            name = extract,
            image = image,
            compact = compact,
            image_layout = "oci_layer_filesystem_applied_changeset",
            layer_layout = "tar",
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
