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
