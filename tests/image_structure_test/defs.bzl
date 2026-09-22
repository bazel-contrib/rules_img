"""Test helpers for image_structure_test."""

load("@rules_img//img:providers.bzl", "ImageManifestInfo")

def _mtree_config_output_groups_impl(ctx):
    info = ctx.attr.image[ImageManifestInfo]
    return [OutputGroupInfo(
        mtree = depset([info.mtree]),
        oci_image_config = depset([info.config]),
    )]

mtree_config_output_groups = rule(
    implementation = _mtree_config_output_groups_impl,
    attrs = {
        "image": attr.label(mandatory = True, providers = [ImageManifestInfo]),
    },
    doc = """Test helper that re-exposes an image's mtree and config JSON as the
`mtree` and `oci_image_config` output groups (and provides nothing else), to
exercise the output-group source of the image_structure_test aspect.""",
)

def _simple_binary_impl(ctx):
    exe = ctx.actions.declare_file("{}/bin/launcher".format(ctx.label.name))
    ctx.actions.symlink(
        output = exe,
        target_file = ctx.file.binary,
        is_executable = True,
    )
    return [DefaultInfo(
        executable = exe,
        files = depset([exe]),
        runfiles = ctx.runfiles(files = ctx.files.data),
    )]

simple_binary = rule(
    implementation = _simple_binary_impl,
    attrs = {
        "binary": attr.label(allow_single_file = True, mandatory = True),
        "data": attr.label_list(allow_files = True),
    },
    executable = True,
    doc = """Minimal executable fixture with runfiles, for layer_from_binary.

Used to exercise the ImageLayerConfigInfo the layer contributes to an image's
config; the executable is never run.""",
)
