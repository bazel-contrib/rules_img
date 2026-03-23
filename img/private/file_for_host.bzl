"""Rule to provide a file built for the host platform."""

load("//img/private/common:transitions.bzl", "host_platform_transition")

def _file_for_host_impl(ctx):
    out = ctx.file.file
    if ctx.attr.symlink:
        out = ctx.actions.declare_file(ctx.attr.name)
        ctx.actions.symlink(
            output = out,
            target_file = ctx.file.file,
        )
    return [DefaultInfo(files = depset([out]))]

file_for_host = rule(
    implementation = _file_for_host_impl,
    attrs = {
        "file": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
        "symlink": attr.bool(
            default = False,
        ),
    },
    cfg = host_platform_transition,
)
