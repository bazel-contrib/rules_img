"""This module implements an alias rule to the resolved toolchain."""

load("//img/private:image_toolchain.bzl", "TOOLCHAIN_TYPE")

DOC = """\
Exposes a concrete toolchain which is the result of Bazel resolving the
toolchain for the execution or target platform.
Workaround for https://github.com/bazelbuild/bazel/issues/14009
"""

# Forward all the providers
def _resolved_toolchain_impl(ctx):
    toolchain_info = ctx.toolchains[TOOLCHAIN_TYPE]
    return [
        toolchain_info,
        toolchain_info.imgtoolchaininfo,
    ]

# Copied from java_toolchain_alias
# https://cs.opensource.google/bazel/bazel/+/master:tools/jdk/java_toolchain_alias.bzl
resolved_toolchain = rule(
    implementation = _resolved_toolchain_impl,
    toolchains = [TOOLCHAIN_TYPE],
    doc = DOC,
)
