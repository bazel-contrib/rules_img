"""Macro for packaging a *_binary target into a container image."""

load("@bazel_features//:features.bzl", "bazel_features")
load("//img/private:index.bzl", "image_index")
load("//img/private:layer_from_binary.bzl", "layer_from_binary")
load("//img/private:manifest.bzl", "image_manifest")

def _image_from_binary_impl(name, binary, path, include_runfiles, layer_budget, layers, kind, platforms, visibility, tags, **kwargs):
    tags = (tags or [])
    intermediate_tags = [] + tags
    if "manual" not in intermediate_tags:
        intermediate_tags.append("manual")
    layer_from_binary(
        name = name + ".layer",
        binary = binary,
        path = path,
        include_runfiles = include_runfiles,
        layer_budget = layer_budget,
        visibility = visibility,
        tags = intermediate_tags,
    )
    layers = layers + [name + ".layer"]
    root_kind = "manifest" if kind == "manifest" or (kind == "auto" and len(platforms) < 2) else "index"
    manifest_name = name if root_kind == "manifest" else name + ".manifest"
    manifest_platform = None if (root_kind == "index" or len(platforms) == 0) else platforms[0]
    image_manifest(
        name = manifest_name,
        layers = layers,
        platform = manifest_platform,
        visibility = visibility,
        tags = tags if root_kind == "manfest" else intermediate_tags,
        **kwargs
    )
    if root_kind == "index":
        image_index(
            name = name,
            manifests = [manifest_name],
            platforms = platforms,
            tags = tags,
            visibility = visibility,
        )

def _image_from_binary_legacy(*, name, binary, path = "", include_runfiles = True, layer_budget = 0, layers = [], kind = "auto", platforms = [], visibility = None, tags = None, **kwargs):
    _image_from_binary_impl(
        name = name,
        binary = binary,
        path = path,
        include_runfiles = include_runfiles,
        layer_budget = layer_budget,
        layers = layers,
        kind = kind,
        platforms = platforms,
        visibility = visibility,
        tags = tags,
        **kwargs
    )

image_from_binary = bazel_features.globals.macro(
    doc = """Packages a *_binary target into a container image.

This is a convenience macro that combines layer_from_binary and image_manifest (or image_index)
into a single target. It is the simplest way to containerize any Bazel `*_binary` target
(Go, C++, Python, Java, Rust, etc.).

The binary's `args`, `env`, and runfiles are automatically extracted and applied to the
image configuration:
- **entrypoint** is set to the binary's path inside the image
- **cmd** is populated from the binary's `args` attribute
- **env** is populated from the binary's `env` attribute (or RunEnvironmentInfo provider)
- **working_dir** is set to the binary's runfiles root

If the binary provides RunfilesGroupInfo (from rules_runfiles_group), the runfiles are split
into separate layers based on the groups. This allows for better caching: stable layers
(interpreter, stdlib) change infrequently and can be shared, while the application code layer
changes with each build. The resolution protocol respects RunfilesGroupTransformInfo and
RunfilesGroupMetadataInfo from the binary's aspect_hints.

All image_manifest attributes (base, env, labels, annotations, etc.) are inherited and
forwarded to the underlying image_manifest. The binary layer is always appended as the
last layer, after any layers specified in the `layers` attribute.

Example:

```python
load("@rules_go//go:def.bzl", "go_binary")
load("@rules_img//img:image.bzl", "image_from_binary")

go_binary(
    name = "server",
    srcs = ["main.go"],
    env = {"GIN_MODE": "release"},
)

# Package the Go binary with a distroless base
image_from_binary(
    name = "app_image",
    binary = ":server",
    base = "@distroless_base",
)

# Custom path and additional layers
image_from_binary(
    name = "full_image",
    binary = "//cmd/server",
    base = "@ubuntu",
    path = "/usr/local/bin/",
    layers = [":config_layer"],
    env = {"LOG_LEVEL": "info"},
)

# Multi-platform image
image_from_binary(
    name = "multiarch_image",
    binary = "//cmd/server",
    base = "@distroless_base",
    platforms = [
        "//:linux_amd64",
        "//:linux_arm64",
    ],
)
```

Targets created:
- `<name>.layer`: The layer_from_binary containing the executable and its runfiles
- `<name>` (or `<name>.manifest` + `<name>` for multi-platform): The image manifest/index
""",
    implementation = _image_from_binary_impl,
    inherit_attrs = image_manifest,
    attrs = {
        "binary": attr.label(
            doc = """The *_binary target to package into the image.

The binary's `args` and `env` attributes are extracted and applied as image configuration
(cmd and env). The `data` attribute is used for `$(location)` expansion in args and env values.

If the binary provides RunfilesGroupInfo, the runfiles are split into separate layers per group.""",
            mandatory = True,
        ),
        "path": attr.string(
            mandatory = False,
            doc = """\
Optional path of the binary inside the image.
If the path ends with a slash ("/"), the basename of the binary will be automatically appended.
If unset, this defaults to the rlocationpath of the binary (e.g., "_main/cmd/server/server_/server").
""",
        ),
        "layers": attr.label_list(
            doc = """Additional layers to include in the image.
The binary layer is automatically appended to the end of this list.""",
        ),
        "include_runfiles": attr.bool(
            default = True,
            doc = """\
Whether to include runfiles for the binary target.
When True (default), the binary's runfiles tree is included and the working directory
is set to the runfiles root. Set to False for statically linked binaries that don't
need runfiles.
""",
        ),
        "layer_budget": attr.int(
            default = 0,
            doc = """\
Maximum number of runfiles group layers.
If set to a value > 0 and the binary provides RunfilesGroupInfo, groups are merged down to this
limit using the merge algorithm from rules_runfiles_group. The algorithm respects group rank
(only merges within the same rank), do_not_merge flags, and weight hints (lighter groups merge first).
0 means no limit (all groups become separate layers).
""",
        ),
        "kind": attr.string(
            default = "auto",
            configurable = False,
            values = ["auto", "manifest", "index"],
            doc = """\
The kind of image to produce.

* "auto": Creates a single-platform manifest if zero or one platforms are provided, otherwise creates an index.
* "manifest": Always creates a single-platform manifest. Fails if multiple platforms are provided.
* "index": Always creates a multi-platform index.
""",
        ),
        "platforms": attr.label_list(
            doc = """\
Target platforms to build the image for.
If empty, the image is built for the current target platform.
If more than one platform is provided, an image_index is automatically created.
""",
            configurable = False,
        ),
        "platform": None,
    },
) if bazel_features.globals.macro != None else _image_from_binary_legacy
