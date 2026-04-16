"""ORAS file layer macro for building OCI artifacts from single files."""

load("@bazel_features//:features.bzl", "bazel_features")
load("//img/private:layer_from_file.bzl", "layer_from_file")

def _oras_file_layer_impl(name, title, annotations, kind, visibility, **kwargs):
    merged_annotations = {}
    diff_id_annotations = []
    if kind == "directory":
        diff_id_annotations.append("io.deis.oras.content.digest")
        merged_annotations["io.deis.oras.content.unpack"] = "true"
    if len(title) > 0:
        merged_annotations["org.opencontainers.image.title"] = title
    merged_annotations.update(annotations)

    layer_from_file(
        name = name,
        annotations = merged_annotations,
        base_name_annotations = ["org.opencontainers.image.title"],
        diff_id_annotations = diff_id_annotations,
        visibility = visibility,
        **kwargs
    )

oras_file_layer = bazel_features.globals.macro(
    implementation = _oras_file_layer_impl,
    inherit_attrs = layer_from_file,
    doc = """Creates an ORAS-compatible layer from a single file.

This macro wraps `layer_from_file` and adds standard ORAS annotations so the
resulting layer can be pushed to and pulled from OCI registries using ORAS
tooling. The `org.opencontainers.image.title` annotation is automatically
set to the base name of the source file (or overridden via the `title` attr).

Two modes are supported via the `kind` attribute:

- `"file"` (default): The source file is used as an opaque blob.
- `"directory"`: The source file is treated as a tar archive. ORAS clients
  will unpack it on pull. The `io.deis.oras.content.unpack` and
  `io.deis.oras.content.digest` annotations are set automatically.

Example:

```python
load("@rules_img//img:oras.bzl", "oras_file_layer")

# Push a single file as an ORAS artifact
oras_file_layer(
    name = "readme_layer",
    src = "README.md",
    media_type = "text/plain",
)

# Push a tar archive that ORAS clients will unpack
oras_file_layer(
    name = "docs_layer",
    src = ":docs.tar.gz",
    kind = "directory",
)
```
""",
    attrs = {
        "title": attr.string(
            default = "",
            configurable = False,
            doc = """Optional override for org.opencontainers.image.title. If left empty, the title is derived from the base name of the blob.""",
        ),
        "kind": attr.string(
            default = "file",
            configurable = False,
            values = ["file", "directory"],
            doc = """The kind of layer. "file" interprets the layer as-is, "directory" interprets the layer blob as a tar file containing a tree.""",
        ),
        "annotations": attr.string_dict(
            default = {},
            configurable = False,
            doc = """Annotations to add to the layer metadata as key-value pairs.""",
        ),
        "base_name_annotations": None,
        "diff_id": None,
        "diff_id_annotations": None,
    },
) if bazel_features.globals.macro != None else None
