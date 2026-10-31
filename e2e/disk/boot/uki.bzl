"""Rule to build a unified kernel image."""

def _uki_impl(ctx):
    # An image_layer also outputs its metadata JSON next to the archive.
    initrds = [f for f in ctx.files.initrd if not f.basename.endswith(".json")]
    if len(initrds) != 1:
        fail("initrd must provide exactly one archive, got %s" % initrds)
    initrd = initrds[0]
    out = ctx.actions.declare_file(ctx.label.name + ".efi")
    args = ctx.actions.args()
    args.add("--stub-tar", ctx.file.stub_tar)
    args.add("--stub-path", ctx.attr.stub_path)
    args.add("--linux", ctx.file.kernel)
    args.add("--initrd", initrd)
    args.add("--cmdline", ctx.attr.cmdline)
    args.add("--osrel", ctx.file.osrel)
    args.add("--output", out)
    ctx.actions.run(
        executable = ctx.executable._uki,
        arguments = [args],
        inputs = [ctx.file.stub_tar, ctx.file.kernel, initrd, ctx.file.osrel],
        outputs = [out],
        mnemonic = "BuildUKI",
        progress_message = "Building unified kernel image %{output}",
    )
    return [DefaultInfo(files = depset([out]))]

uki = rule(
    implementation = _uki_impl,
    doc = """Builds a unified kernel image: a UEFI stub with the kernel, initrd, command line and os-release as sections.""",
    attrs = {
        "cmdline": attr.string(doc = "The kernel command line."),
        "initrd": attr.label(allow_files = True, mandatory = True, doc = "The initrd (a cpio archive)."),
        "kernel": attr.label(allow_single_file = True, mandatory = True, doc = "The kernel image (built with an EFI stub)."),
        "osrel": attr.label(allow_single_file = True, mandatory = True, doc = "The os-release file that describes the image."),
        "stub_path": attr.string(mandatory = True, doc = "Path of the UEFI stub inside stub_tar."),
        "stub_tar": attr.label(allow_single_file = True, mandatory = True, doc = "Tar archive with the UEFI stub."),
        "_uki": attr.label(
            default = "//tools/uki",
            executable = True,
            cfg = "exec",
        ),
    },
)
