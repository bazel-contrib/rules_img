"""Analysis tests for the treeartifact_symlinks setting.

Pins that the directory-producing layout actions pass `--symlink` by default,
drop it when //img/settings:treeartifact_symlinks is disabled, and never pass it
for the tar-format layouts.
"""

load("@bazel_features//:features.bzl", "bazel_features")
load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")

_SYMLINKS_FLAG = str(Label("//img/settings:treeartifact_symlinks"))

# Mnemonics of the actions that assemble a layout of blobs. Each runs once per
# output format, so the format is read back off the action's argv.
_LAYOUT_MNEMONICS = ["OCILayout", "SparseOCILayout"]

def _format_of(argv):
    for i in range(len(argv) - 1):
        if argv[i] == "--format":
            return argv[i + 1]
    return None

def _symlink_args_impl(ctx):
    env = analysistest.begin(ctx)
    directory_actions = 0
    for action in analysistest.target_actions(env):
        if action.mnemonic not in _LAYOUT_MNEMONICS:
            continue
        format = _format_of(action.argv)
        if format == "directory":
            directory_actions += 1
        asserts.equals(
            env,
            ctx.attr.expect_symlink if format == "directory" else False,
            "--symlink" in action.argv,
            "{} action ({} format): --symlink in {}".format(action.mnemonic, format, action.argv),
        )
    asserts.true(env, directory_actions >= 2, "expected directory-format layout actions, found {}".format(directory_actions))
    return analysistest.end(env)

_ATTRS = {"expect_symlink": attr.bool()}

_symlink_args_test = analysistest.make(
    _symlink_args_impl,
    attrs = _ATTRS,
)

_symlink_args_disabled_test = analysistest.make(
    _symlink_args_impl,
    attrs = _ATTRS,
    config_settings = {_SYMLINKS_FLAG: "disabled"},
)

def treeartifact_symlinks_test_suite(name, image):
    """Declares the analysis tests.

    Args:
        name: Name of the test suite.
        image: An image_manifest target to inspect the actions of.
    """
    _symlink_args_test(
        name = name + "_default",
        target_under_test = image,
        # The host is the exec platform here, and Windows never gets symlinks.
        expect_symlink = select({
            "@platforms//os:windows": False,
            "//conditions:default": bazel_features.rules.permits_treeartifact_uplevel_symlinks,
        }),
        size = "small",
    )
    _symlink_args_disabled_test(
        name = name + "_disabled",
        target_under_test = image,
        expect_symlink = False,
        size = "small",
    )
    native.test_suite(
        name = name,
        tests = [":" + name + "_default", ":" + name + "_disabled"],
    )
