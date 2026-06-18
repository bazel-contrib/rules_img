"""Repository rule that builds the img tool from source using the Go toolchain.

This is used to bootstrap a host binary of the img tool inside a repository rule
context (i.e. at fetch time, before the build graph exists). Repository rules that
pull container manifests and blobs need the img tool available before any build
action runs, so it cannot be provided as a regular build target.
"""

load("@go_host_compatible_sdk_label//:defs.bzl", "HOST_COMPATIBLE_SDK")

def _img_bootstrap_impl(rctx):
    go_sdk_label = Label("@" + rctx.attr._go_sdk_name + "//:ROOT")
    go_root = str(rctx.path(go_sdk_label).dirname)
    extension = _executable_extension(rctx)
    go_tool = go_root + "/bin/go" + extension
    go_mod = rctx.path(rctx.attr.go_mod)
    go_sum = rctx.path(rctx.attr.go_sum)
    src_root = rctx.path(go_mod).dirname
    cmd_dir = src_root.get_child("cmd")
    pkg_dir = src_root.get_child("pkg")
    rctx.watch(go_tool)
    rctx.watch(go_mod)
    rctx.watch(go_sum)
    rctx.watch_tree(cmd_dir)
    rctx.watch_tree(pkg_dir)
    rctx.symlink(go_mod, "go.mod")
    rctx.symlink(go_sum, "go.sum")
    rctx.symlink(cmd_dir, "cmd")
    rctx.symlink(pkg_dir, "pkg")
    args = [
        go_tool,
        "build",
        "-o",
        rctx.path("./img.exe"),
        "-ldflags=-s -w",
        "-trimpath",
        "./cmd/img",
    ]
    exec_result = rctx.execute(
        args,
        environment = {
            "CGO_ENABLED": "0",
        },
    )
    if exec_result.return_code != 0:
        fail("go build failed {}: {}{}".format(args, exec_result.stderr, exec_result.stdout))
    rctx.file(
        "BUILD.bazel",
        """exports_files(["img.exe"])""",
    )

    # cleanup symlinks
    rctx.delete("go.mod")
    rctx.delete("go.sum")
    rctx.delete("cmd")
    rctx.delete("pkg")

    if hasattr(rctx, "repo_metadata"):
        # allows participating in repo contents cache
        return rctx.repo_metadata(reproducible = True)

    # only to make buildifier happy
    return None

img_bootstrap = repository_rule(
    implementation = _img_bootstrap_impl,
    attrs = {
        "go_mod": attr.label(mandatory = True),
        "go_sum": attr.label(mandatory = True),
        "_go_sdk_name": attr.string(default = "@" + HOST_COMPATIBLE_SDK.repo_name),
    },
)

def _executable_extension(rctx):
    extension = ""
    if rctx.os.name.startswith("windows"):
        extension = ".exe"
    return extension
