"""Rule to build a unified kernel image."""

load("@rules_img//img:providers.bzl", "LayersInfo")

def _uki_impl(ctx):
    # A layer rule outputs its metadata next to the archive, so take the blob
    # from the provider instead of guessing which of the files it is.
    layers = ctx.attr.initrd[LayersInfo].layers
    if len(layers) != 1:
        fail("{}: initrd must be a layer target that produces exactly one layer, but {} produces {}.".format(ctx.label, ctx.attr.initrd.label, len(layers)))
    initrd = layers[0].blob
    if initrd == None:
        fail("{}: the initrd layer {} has no blob. A layer built as a compact stream needs --@rules_img//img/settings:experimental_compact_layers_materialize_blob=enabled.".format(ctx.label, ctx.attr.initrd.label))
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
        "initrd": attr.label(mandatory = True, providers = [LayersInfo], doc = "The initrd: a layer target with a single layer (a cpio archive)."),
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
