"""Test helpers for image_size_test."""

load("@rules_img//img:providers.bzl", "ImageManifestInfo", "SingleLayerInfo")

def _opaque_layer_impl(ctx):
    layer = SingleLayerInfo(
        blob = None,
        metadata = ctx.file.metadata,
        media_type = ctx.attr.media_type,
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

opaque_layer_image = rule(
    implementation = _opaque_layer_impl,
    attrs = {
        "metadata": attr.label(allow_single_file = True, mandatory = True),
        "media_type": attr.string(
            doc = """Media type of the single layer.

A tar type models a pulled base-image layer that is never fetched; the empty
string models a provider that does not record the field at all. Either way the
layer's filesystem content is unknown.""",
            default = "application/vnd.oci.image.layer.v1.tar+gzip",
        ),
    },
    doc = """Minimal ImageManifestInfo stand-in whose only layer exposes no
filesystem content (no blob, no mtree, no compact stream). image_size_test must
fail on it with an "incomplete layer metadata" error instead of silently passing
on a subset of the image.""",
)
