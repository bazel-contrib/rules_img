"""Test helpers for image_size_test."""

load("@rules_img//img:providers.bzl", "ImageManifestInfo", "SingleLayerInfo")

def _shallow_image_impl(ctx):
    layer = SingleLayerInfo(
        blob = None,
        metadata = ctx.file.metadata,
        media_type = "application/vnd.oci.image.layer.v1.tar+gzip",
        estargz = False,
        compact_stream = None,
        layer_input_files = None,
        layer_input_files_cas = None,
        sources = [],
        mtree = None,
        ztoc = None,
    )
    return [ImageManifestInfo(
        architecture = "arm64",
        os = "linux",
        variant = "",
        layers = [layer],
    )]

shallow_image = rule(
    implementation = _shallow_image_impl,
    attrs = {
        "metadata": attr.label(allow_single_file = True, mandatory = True),
    },
    doc = """Minimal ImageManifestInfo stand-in whose only layer is shallow
(no blob, no mtree), like a pulled base image layer that is never fetched.
image_size_test must fail on it with an "incomplete layer metadata" error
instead of silently passing on a subset of the image.""",
)
