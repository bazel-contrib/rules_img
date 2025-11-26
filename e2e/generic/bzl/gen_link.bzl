"""A rule to generate a symlink to directory"""

load("@bazel_skylib//lib:paths.bzl", "paths")

def _gen_dir_link_impl(ctx):
    out_dir = ctx.actions.declare_directory("out_dir")
    ctx.actions.run_shell(
        outputs = [out_dir],
        command = """
            mkdir -p -- "$1"
            echo 42 > "$1/1.txt"
        """,
        arguments = [out_dir.path],
    )

    out_files = [out_dir]

    # in Bazel 7 it is not possible to check for symlinks, so don't create one
    if hasattr(out_dir, "is_symlink"):
        out_link = ctx.actions.declare_symlink("out_link")
        ctx.actions.run_shell(
            inputs = [out_dir],
            outputs = [out_link],
            command = """
                ln -s -- "$1" "$2"
            """,
            arguments = [
                paths.relativize(out_dir.path, paths.dirname(out_link.path)),
                out_link.path,
            ],
        )
        out_files.append(out_link)

    return [DefaultInfo(
        files = depset(out_files),
    )]

gen_dir_link = rule(
    implementation = _gen_dir_link_impl,
)
