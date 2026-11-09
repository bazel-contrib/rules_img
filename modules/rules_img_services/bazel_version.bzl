"""A repository rule that exposes the version of Bazel that is running.

`native.bazel_version` can only be read while a repository rule runs, not in a
BUILD file or a macro, so the version is written to a `version.bzl` that those can
load. The same trick as bazel_features'.
"""

def _bazel_version_impl(rctx):
    rctx.file("BUILD.bazel", "exports_files([\"version.bzl\"])\n")
    rctx.file("version.bzl", "version = {}\n".format(repr(native.bazel_version)))

bazel_version = repository_rule(
    implementation = _bazel_version_impl,
    doc = "Creates a repository with `version.bzl`, defining `version`: the version of Bazel in use.",
    # Run again when the Bazel server restarts, so that the version stays current
    # after an upgrade.
    local = True,
)
