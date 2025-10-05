"""Repository rule that builds pull_tool using Go binary"""

load("@go_host_compatible_sdk_label//:defs.bzl", "HOST_COMPATIBLE_SDK")

def _pull_bootstrap_impl(rctx):
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
    for src in rctx.attr.pull_tool_srcs:
        rctx.watch(src)
    args = [
        go_tool,
        "build",
        "-o",
        rctx.path("./pull_tool.exe"),
        "-ldflags=-s -w",
        "-trimpath",
        "./cmd/pull_tool",
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
        """exports_files(["pull_tool.exe"])""",
    )

pull_bootstrap = repository_rule(
    implementation = _pull_bootstrap_impl,
    attrs = {
        "pull_tool_srcs": attr.label_list(mandatory = True),
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
