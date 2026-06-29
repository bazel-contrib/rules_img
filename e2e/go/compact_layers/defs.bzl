"""Rule for generating pseudorandom data files at build time."""

def _gen_pseudorandom_data_impl(ctx):
    output = ctx.actions.declare_file(ctx.attr.name + ".bin")
    ctx.actions.run(
        inputs = [],
        outputs = [output],
        executable = ctx.executable._gen_data,
        arguments = [
            "--seed",
            str(ctx.attr.seed),
            "--size",
            str(ctx.attr.size),
            "--output",
            output.path,
        ],
        mnemonic = "GenPseudorandomData",
    )
    return [DefaultInfo(files = depset([output]))]

gen_pseudorandom_data = rule(
    implementation = _gen_pseudorandom_data_impl,
    doc = "Generates a file of pseudorandom bytes deterministically from a seed.",
    attrs = {
        "seed": attr.int(
            default = 42,
            doc = "Random seed for deterministic output.",
        ),
        "size": attr.int(
            default = 1024,
            doc = "Output size in bytes.",
        ),
        "_gen_data": attr.label(
            default = Label("//compact_layers/gen_data:gen_data"),
            executable = True,
            cfg = "exec",
        ),
    },
)
