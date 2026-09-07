"""Implementation of the image_toolchain rule."""

load("@bazel_features//:features.bzl", "bazel_features")
load("//img/private/common:transitions.bzl", "reset_platform_transition")
load("//img/private/providers:image_toolchain_info.bzl", "ImageToolchainInfo")

DOC = """\
Defines an image builder toolchain.

The image build tool can natively target any platform,
so it only has exec platform constraints.

See https://bazel.build/extending/toolchains#defining-toolchains.
"""

ATTRS = dict(
    exec_os = attr.string(
        doc = "The GOOS of the tool executable. Must match the OS in the registering toolchain's exec_compatible_with constraints.",
        mandatory = True,
    ),
    tool_exe = attr.label(
        doc = "An image build tool executable.",
        allow_single_file = True,
    ),
)

TOOLCHAIN_TYPE = str(Label("//img:toolchain_type"))
DATA_TOOLCHAIN_TYPE = str(Label("//img:data_toolchain_type"))

def _image_toolchain_impl(ctx):
    # Use the OS of the registered tool, not the target or host platform: the
    # selected execution platform may differ from both when cross-compiling.
    # Prebuilt tools are named img.exe on every OS, so the suffix is not useful.
    # Windows hardlinks can propagate relative symlink targets unchanged when
    # copying files out of a tree artifact, leaving dangling symlinks.
    image_toolchain_info = ImageToolchainInfo(
        supports_treeartifact_uplevel_symlinks = (
            ctx.attr.exec_os != "windows" and
            bazel_features.rules.permits_treeartifact_uplevel_symlinks
        ),
        tool_exe = ctx.file.tool_exe,
    )
    toolchain_info = platform_common.ToolchainInfo(
        imgtoolchaininfo = image_toolchain_info,
    )

    return [toolchain_info]

image_toolchain = rule(
    implementation = _image_toolchain_impl,
    attrs = ATTRS,
    doc = DOC,
    cfg = reset_platform_transition,
)
