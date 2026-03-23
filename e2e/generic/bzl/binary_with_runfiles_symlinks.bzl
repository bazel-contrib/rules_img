"""A rule to generate a binary with runfiles symlinks and root_symlinks"""

def _binary_with_runfiles_symlinks_impl(ctx):
    executable = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(
        output = executable,
        target_file = ctx.file.binary,
        is_executable = True,
    )
    runfiles = ctx.runfiles(
        files = ctx.files.data,
        symlinks = {
            name: target[DefaultInfo].files.to_list()[0]
            for name, target in ctx.attr.symlinks.items()
        },
        root_symlinks = {
            name: target[DefaultInfo].files.to_list()[0]
            for name, target in ctx.attr.root_symlinks.items()
        },
    )
    transitive_runfiles = []
    transitive_runfiles.append(ctx.attr.binary[DefaultInfo].default_runfiles)
    for runfiles_attr in (
        ctx.attr.data,
    ):
        for target in runfiles_attr:
            transitive_runfiles.append(target[DefaultInfo].default_runfiles)
    runfiles = runfiles.merge_all(transitive_runfiles)
    return [DefaultInfo(
        files = depset([executable]),
        runfiles = runfiles,
        executable = executable,
    )]

binary_with_runfiles_symlinks = rule(
    implementation = _binary_with_runfiles_symlinks_impl,
    attrs = {
        "binary": attr.label(
            allow_single_file = True,
            cfg = "target",
        ),
        "data": attr.label_list(
            allow_files = True,
        ),
        "symlinks": attr.string_keyed_label_dict(
            allow_files = True,
        ),
        "root_symlinks": attr.string_keyed_label_dict(
            allow_files = True,
        ),
    },
    executable = True,
)
