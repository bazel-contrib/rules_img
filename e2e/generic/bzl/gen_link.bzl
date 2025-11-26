"""A rule to generate a symlink to directory"""

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

    out_link = ctx.actions.declare_symlink("out_link")
    ctx.actions.run_shell(
        inputs = [out_dir],
        outputs = [out_link],
        command = """
            ln -sr -T "$1" -- "$2"
        """,
        arguments = [out_dir.path, out_link.path],
    )

    return [DefaultInfo(
        files = depset([out_dir, out_link]),
    )]

gen_dir_link = rule(
    implementation = _gen_dir_link_impl,
)
