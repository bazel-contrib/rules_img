"""Rules to build a unified kernel image and an initrd."""

def _uki_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".efi")
    args = ctx.actions.args()
    args.add("--stub-tar", ctx.file.stub_tar)
    args.add("--stub-path", ctx.attr.stub_path)
    args.add("--linux", ctx.file.kernel)
    args.add("--initrd", ctx.file.initrd)
    args.add("--cmdline", ctx.attr.cmdline)
    args.add("--osrel", ctx.file.osrel)
    args.add("--output", out)
    ctx.actions.run(
        executable = ctx.executable._uki,
        arguments = [args],
        inputs = [ctx.file.stub_tar, ctx.file.kernel, ctx.file.initrd, ctx.file.osrel],
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
        "initrd": attr.label(allow_single_file = True, mandatory = True, doc = "The initrd (a cpio archive)."),
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

def _initrd_impl(ctx):
    out = ctx.actions.declare_file(ctx.label.name + ".cpio.gz")
    ctx.actions.run(
        executable = ctx.executable._tar2cpio,
        arguments = [ctx.file.tar.path, out.path],
        inputs = [ctx.file.tar],
        outputs = [out],
        mnemonic = "BuildInitrd",
        progress_message = "Building initrd %{output}",
    )
    return [DefaultInfo(files = depset([out]))]

initrd = rule(
    implementation = _initrd_impl,
    doc = "Converts a tar archive into a gzip-compressed cpio archive, the format of an initramfs.",
    attrs = {
        "tar": attr.label(allow_single_file = True, mandatory = True, doc = "The tar archive with the content of the initrd."),
        "_tar2cpio": attr.label(
            default = "//tools/tar2cpio",
            executable = True,
            cfg = "exec",
        ),
    },
)
