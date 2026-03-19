"""Rule for producing an empty OCI layer with application/vnd.oci.empty.v1+json media type."""

load("//img/private/providers:layer_info.bzl", "LayerInfo")

def _empty_layer_impl(ctx):
    blob_data = "{}"
    media_type = "application/vnd.oci.empty.v1+json"
    metadata = dict(
        name = "empty",
        mediaType = media_type,
        digest = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
        size = len(blob_data),
    )
    out = ctx.actions.declare_file(ctx.attr.name + ".json")
    metadata_out = ctx.actions.declare_file(ctx.attr.name + "_metadata.json")
    ctx.actions.write(out, blob_data)
    ctx.actions.write(metadata_out, json.encode(metadata))
    return [
        DefaultInfo(files = depset([out])),
        OutputGroupInfo(
            layer = depset([out]),
            metadata = depset([metadata_out]),
        ),
        LayerInfo(
            blob = out,
            metadata = metadata_out,
            media_type = media_type,
            estargz = False,
        ),
    ]

empty_layer = rule(
    implementation = _empty_layer_impl,
    provides = [LayerInfo],
)
