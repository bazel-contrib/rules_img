"""Layer rule for using arbitrary files as layers."""

load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:layer_helper.bzl", "calculate_layer_info")
load("//img/private/providers:layer_info.bzl", "LayerInfo")

def _layer_from_file_impl(ctx):
    media_type = ctx.attr.media_type
    if media_type == "":
        media_type = "application/vnd.oci.image.layer.v1.tar"

    metadata_file = ctx.actions.declare_file("{}_metadata.json".format(ctx.attr.name))

    digest_modes = ["digest"]
    if ctx.attr.diff_id:
        digest_modes.append("diff_id")
    for annotation_name in ctx.attr.diff_id_annotations:
        if ":" in annotation_name:
            fail("Invalid annotation name (may not contain a colon): ", annotation_name)
        digest_modes.append("diff_id_annotation:" + annotation_name)

    annotations = {}
    for annotation_name in ctx.attr.base_name_annotations:
        annotations[annotation_name] = ctx.file.src.basename
    annotations.update(ctx.attr.annotations)

    layer_info = calculate_layer_info(
        ctx = ctx,
        media_type = media_type,
        tar_file = ctx.file.src,
        metadata_file = metadata_file,
        estargz = False,
        annotations = annotations,
        digest_modes = digest_modes,
    )

    return [
        DefaultInfo(
            files = depset([layer_info.blob, layer_info.metadata]),
        ),
        OutputGroupInfo(
            layer = depset([layer_info.blob]),
            metadata = depset([layer_info.metadata]),
        ),
        layer_info,
    ]

layer_from_file = rule(
    implementation = _layer_from_file_impl,
    doc = """Creates a container image layer from an arbitrary file.

This rule uses any file as a container image layer blob, computing the
necessary metadata (digest, size) without any tar-specific processing
like compression or optimization.

This is useful for non-tar layer content such as Helm charts, WASM
modules, or other OCI artifacts.

If you want to use an existing tar file as a layer, use layer_from_tar instead.

Example:

```python
load("@rules_img//img:layer.bzl", "layer_from_file")

# Use an arbitrary artifact as a layer
layer_from_file(
    name = "artifact_layer",
    src = ":artifact.bin",
    media_type = "application/octet-stream",
    annotations = {
        "org.opencontainers.image.title": "artifact.bin",
    },
)
```
""",
    attrs = {
        "src": attr.label(
            mandatory = True,
            allow_single_file = True,
            doc = """The file to use as a layer blob.""",
        ),
        "annotations": attr.string_dict(
            default = {},
            doc = """Annotations to add to the layer metadata as key-value pairs.""",
        ),
        "base_name_annotations": attr.string_list(
            default = [],
            doc = "List of annotations that are set to the basename of the file.",
        ),
        "media_type": attr.string(
            default = "application/vnd.oci.image.layer.v1.tar",
            doc = """Layer media type. Defaults to "application/vnd.oci.image.layer.v1.tar" if not set.""",
        ),
        "diff_id": attr.bool(
            default = False,
            doc = "If set, interprets the file as a (potentially compressed) tar file and calculates the diff_id. Warning: if you do this, you probably want to use layer_from_tar instead.",
        ),
        "diff_id_annotations": attr.string_list(
            default = [],
            doc = "List of annotations that are set to the diff_id of the file. Only works with tar files.",
        ),
    },
    toolchains = TOOLCHAINS,
    provides = [LayerInfo],
)
