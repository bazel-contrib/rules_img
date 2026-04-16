"""ORAS layer macro for building OCI artifacts from files and directories."""

load("@bazel_features//:features.bzl", "bazel_features")
load("//img/private:layer.bzl", "image_layer")

def _oras_layer_impl(name, title, annotations, visibility, **kwargs):
    merged_annotations = {
        "io.deis.oras.content.unpack": "true",
        # Secret handshake with the image_layer rule:
        # If we set the special DERIVE_FROM_DIFF_ID sentinel value,
        # it is replaced with the actual diff_id after producing the layer.
        "io.deis.oras.content.digest": "DERIVE_FROM_DIFF_ID",
    }
    if len(title) == 0:
        title = name
    merged_annotations["org.opencontainers.image.title"] = title
    merged_annotations.update(annotations)

    image_layer(
        name = name,
        annotations = merged_annotations,
        visibility = visibility,
        **kwargs
    )

oras_layer = bazel_features.globals.macro(
    implementation = _oras_layer_impl,
    inherit_attrs = image_layer,
    doc = """Creates an ORAS-compatible layer from files and directories.

This macro wraps `image_layer` and adds standard ORAS annotations so the
resulting layer can be pushed to and pulled from OCI registries using ORAS
tooling:

- `org.opencontainers.image.title` is set to the `title` attribute (defaults
  to the target name).
- `io.deis.oras.content.unpack` is set to `"true"` so ORAS clients unpack the
  layer on pull.
- `io.deis.oras.content.digest` is automatically derived from the layer's diff
  ID after the tar is produced.

Example:

```python
load("@rules_img//img:oras.bzl", "oras_layer")

# Package application files as an ORAS artifact
oras_layer(
    name = "app_layer",
    srcs = {
        "/app/bin/server": "//cmd/server",
        "/app/config.json": ":config",
    },
)
```
""",
    attrs = {
        "title": attr.string(
            default = "",
            configurable = False,
            doc = """Optional value for org.opencontainers.image.title. If left empty, the title is derived from the target name.""",
        ),
        "annotations": attr.string_dict(
            default = {},
            configurable = False,
            doc = """Annotations to add to the layer metadata as key-value pairs.""",
        ),
    },
) if bazel_features.globals.macro != None else None
