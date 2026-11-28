"""Tests for the cas_input_list argument of create_single_layer.

`_cas_input_list_layer` builds a compact layer from `packed` while also declaring
`unpacked` as a layer input, the way a rule that over-declares inputs would. With
`use_list`, it passes a `cas_input_list` naming only `packed`. Its output lists
the blob names in the layer's content-addressed input directory, and `diff_test`
compares that list with the expected digests. With the list, only the blob for
`packed` should be there. Without it, the blob for `unpacked` is there too.
"""

load("@bazel_skylib//rules:diff_test.bzl", "diff_test")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:layer_attrs.bzl", "layer_attrs")
load("//img/private/common:tar_layer.bzl", "create_single_layer", "files_arg", "resolve_layer_settings")

_COMPACT_LAYERS_SETTING = str(Label("//img/settings:experimental_compact_layers"))

# sha256 of "packed\n" and "unpacked\n", the contents written below.
_PACKED_SHA256 = "d535a32ae4ccd17fb41897c4ba5077d9e1aaa9ae82e6ad990e2c93b0dd10821d"
_UNPACKED_SHA256 = "3617973fc3463ef039504affd436824bc1d6f5572b10acf057d45f610a6844ac"

def _enable_compact_layers_impl(_settings, _attr):
    return {_COMPACT_LAYERS_SETTING: "enabled"}

_enable_compact_layers = transition(
    implementation = _enable_compact_layers_impl,
    inputs = [],
    outputs = [_COMPACT_LAYERS_SETTING],
)

def _blob_name(f):
    return f.basename

def _cas_input_list_layer_impl(ctx):
    settings = resolve_layer_settings(ctx)
    if not settings.compact_layers:
        fail("_cas_input_list_layer needs compact layers")
    packed = ctx.file.packed

    add_args = ctx.actions.args()
    add_args.set_param_file_format("multiline")
    add_args.use_param_file("--add-from-file=%s", use_always = True)
    add_args.add_all([packed], map_each = files_arg, format_each = "packed.txt\0%s")

    cas_input_list = None
    if ctx.attr.use_list:
        cas_input_list = ctx.actions.declare_file(ctx.label.name + "_cas_input_list.txt")
        list_args = ctx.actions.args()
        list_args.set_param_file_format("multiline")
        list_args.add_all([packed])
        ctx.actions.write(cas_input_list, list_args)

    layer_info = create_single_layer(
        ctx,
        settings,
        ctx.label.name,
        extra_args = [add_args],
        extra_inputs = [depset([packed, ctx.file.unpacked])],
        cas_input_list = cas_input_list,
    )[0]

    blobs = ctx.actions.declare_file(ctx.label.name + "_blobs.txt")
    blob_args = ctx.actions.args()
    blob_args.set_param_file_format("multiline")
    blob_args.add_all([layer_info.layer_input_files_cas], map_each = _blob_name, expand_directories = True)
    ctx.actions.write(blobs, blob_args)
    return [DefaultInfo(files = depset([blobs]))]

_cas_input_list_layer = rule(
    implementation = _cas_input_list_layer_impl,
    cfg = _enable_compact_layers,
    attrs = {
        "packed": attr.label(allow_single_file = True, mandatory = True),
        "unpacked": attr.label(allow_single_file = True, mandatory = True),
        "use_list": attr.bool(),
    } | layer_attrs.common,
    toolchains = TOOLCHAINS,
)

def cas_input_list_test_suite(name):
    """Declare tests for the cas_input_list argument of create_single_layer.

    Args:
        name: Name for the test suite.
    """
    packed = name + "_packed"
    write_file(
        name = packed,
        out = packed + ".txt",
        content = ["packed\n"],
        tags = ["manual"],
    )
    unpacked = name + "_unpacked"
    write_file(
        name = unpacked,
        out = unpacked + ".txt",
        content = ["unpacked\n"],
        tags = ["manual"],
    )

    tests = []

    # Blob names come out in path order (sha256/<xx>/<hex>), hence the order of
    # the expected lines.
    for case, use_list, expected in [
        ("with_list", True, [_PACKED_SHA256]),
        ("without_list", False, [_UNPACKED_SHA256, _PACKED_SHA256]),
    ]:
        subject = "{}_{}".format(name, case)
        _cas_input_list_layer(
            name = subject,
            packed = ":" + packed,
            unpacked = ":" + unpacked,
            use_list = use_list,
            tags = ["manual"],
        )
        want = subject + "_expected"
        write_file(
            name = want,
            out = want + ".txt",
            content = expected + [""],
            newline = "unix",
            tags = ["manual"],
        )
        test = subject + "_test"
        diff_test(
            name = test,
            size = "small",
            file1 = ":" + subject,
            file2 = ":" + want,
        )
        tests.append(":" + test)

    native.test_suite(
        name = name,
        tests = tests,
    )
