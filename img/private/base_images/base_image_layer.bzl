"""Rule turning base image content descriptions into a single flat layer."""

load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:layer_attrs.bzl", "layer_attrs")
load("//img/private/common:tar_layer.bzl", "create_tar_layer", "resolve_layer_settings")
load("//img/private/providers:base_image_content_info.bzl", "BaseImageContentInfo")
load("//img/private/providers:layers_info.bzl", "LayersInfo")

def _base_image_layer_impl(ctx):
    settings = resolve_layer_settings(ctx)

    # Order matters: for a path described by more than one src, the last one
    # wins. "preorder" is the only order that preserves it -- the default order
    # makes no promise about how transitive depsets interleave, and "postorder"
    # would put a src's own dependencies after it, inverting the override.
    metadata = depset(
        order = "preorder",
        transitive = [src[BaseImageContentInfo].metadata for src in ctx.attr.srcs],
    )
    referenced_files = depset(
        transitive = [src[BaseImageContentInfo].files for src in ctx.attr.srcs],
    )

    extra_args = []
    if ctx.attr.default_metadata:
        extra_args.extend(["--default-metadata", ctx.attr.default_metadata])
    for path, metadata_json in ctx.attr.file_metadata.items():
        path = path.removeprefix("/")  # the "/" is not included in the tar file.
        extra_args.extend(["--file-metadata", "{}={}".format(path, metadata_json)])

    streams_args = ctx.actions.args()
    streams_args.set_param_file_format("multiline")
    streams_args.use_param_file("--base-metadata-from-file=%s", use_always = True)
    streams_args.add_all(metadata)
    extra_args.append(streams_args)

    return create_tar_layer(
        ctx,
        settings,
        extra_args = extra_args,
        extra_inputs = [metadata, referenced_files],
    )

base_image_layer = rule(
    implementation = _base_image_layer_impl,
    doc = """Builds a single flat container image layer from base image content descriptions.

Takes any number of content-describing targets -- `linux_skeleton`, `etc_passwd`,
`trust_store` and the rest -- and merges everything they describe into one
layer. Nothing is materialized until this point: the content rules only
accumulate tar entry metadata, which propagates cheaply through dependencies.

For a path described by more than one `src`, the last one wins, so a rule can
override what an earlier one placed. Two entries for the same path with
different *types* (a directory where another src put a symlink) are an error
instead: that is a rule authoring mistake rather than a deliberate override.

The resulting layer is an ordinary rules_img layer and supports the same
compression, eStargz, SOCI and compact-stream settings as `image_layer`.

Example:

```python
load(
    "@rules_img//img:base_images.bzl",
    "base_image_layer",
    "etc_passwd",
    "linux_skeleton",
    "passwd_entry",
    "trust_store",
)
load("@rules_img//img:image.bzl", "image_manifest")

linux_skeleton(name = "skeleton")

etc_passwd(
    name = "users",
    users = [passwd_entry(username = "app", uid = 1000, gid = 1000, home = "/home/app")],
)

trust_store(
    name = "trust",
    certs = ["//pki:corporate-root.pem"],
)

base_image_layer(
    name = "base_layer",
    srcs = [
        ":skeleton",
        ":users",
        ":trust",
    ],
)

image_manifest(
    name = "base",
    layers = [":base_layer"],
)
```

### Output groups

- `layer`: the compressed layer blob
- `metadata`: the layer's OCI descriptor as JSON
- `mtree`: a single [mtree](https://man.freebsd.org/cgi/man.cgi?mtree(5)) text file
""",
    attrs = {
        "srcs": attr.label_list(
            doc = """Base image content descriptions to merge into the layer.

Order matters: for a path described by more than one src, the last one wins.""",
            providers = [BaseImageContentInfo],
        ),
        "default_metadata": attr.string(
            default = "",
            doc = """JSON-encoded default metadata for entries that do not set it themselves.

Only the modification time is taken from here: an entry's own mode and ownership
are authoritative, since deciding them is the entire purpose of the rule that
produced it. Build the value with `file_metadata()` from
`@rules_img//img:layer.bzl`.""",
        ),
        "file_metadata": attr.string_dict(
            default = {},
            doc = """Per-file metadata overrides, mapping image path to JSON-encoded metadata.

Unlike `default_metadata`, an override here replaces whatever the producing rule
chose. Use it to adjust a single entry without changing the rule that describes
it.""",
        ),
    } | layer_attrs.common,
    toolchains = TOOLCHAINS,
    provides = [LayersInfo],
)
