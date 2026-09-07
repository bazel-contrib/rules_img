"""Regression tests for toolchain tree-artifact symlink capabilities."""

load("@bazel_features//:features.bzl", "bazel_features")
load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@bazel_skylib//rules:write_file.bzl", "write_file")
load("//img/private:image_toolchain.bzl", "image_toolchain")

def _capability_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[platform_common.ToolchainInfo].imgtoolchaininfo
    asserts.equals(env, ctx.attr.expected, info.supports_treeartifact_uplevel_symlinks)
    return analysistest.end(env)

_capability_test = analysistest.make(
    _capability_test_impl,
    attrs = {"expected": attr.bool()},
)

def image_toolchain_test_suite(name):
    """Checks execution OS handling independently of executable filename.

    Args:
        name: Name of the test suite.
    """
    tests = []
    for exec_os in ["darwin", "linux", "windows"]:
        for suffix in ["", ".exe"]:
            case = name + "_" + exec_os + ("_exe" if suffix else "_no_suffix")
            write_file(
                name = case + "_file",
                out = case + "/img" + suffix,
                content = ["unused: analysis-only fixture"],
                tags = ["manual"],
            )
            image_toolchain(
                name = case + "_toolchain",
                exec_os = exec_os,
                tool_exe = ":" + case + "_file",
                tags = ["manual"],
            )
            _capability_test(
                name = case,
                target_under_test = ":" + case + "_toolchain",
                expected = exec_os != "windows" and bazel_features.rules.permits_treeartifact_uplevel_symlinks,
                size = "small",
            )
            tests.append(":" + case)
    native.test_suite(name = name, tests = tests)
