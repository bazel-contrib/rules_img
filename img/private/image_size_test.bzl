"""`image_size_test`: budget wasted and duplicated bytes across an image's layers.

An aspect on the `image` attribute collects the image's per-layer mtrees (in
layer order, from `ImageManifestInfo.layers`) plus each layer's metadata file
(whose history `created_by` attributes a layer to its producing target where
possible), and the test rule wraps the `img image-size-test` subcommand with
the hermetic launcher. Only mtree + metadata files reach the test runfiles --
never layer blobs.

Unlike `image_structure_test`, this needs a per-layer (not merged) view of the
image, so sources that only expose a merged mtree (the `mtree` output group or
an OCI image layout directory) are rejected in the aspect.
"""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@hermetic_launcher//launcher:lib.bzl", "launcher")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:layer_helper.bzl", "build_layer_mtree", "media_type_is_tar")
load("//img/private/providers:deploy_tool_info.bzl", "DeployToolInfo")
load("//img/private/providers:image_size_test_info.bzl", "ImageSizeTestInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")

# mtree rendering settings for the on-the-fly per-layer mtrees the aspect
# builds for blob-bearing layers that carry none (raw tars added via
# DefaultInfo); see build_layer_mtree.
_ASPECT_MTREE_ATTRS = {
    "_mtree_path_prefix": attr.label(
        default = Label("//img/settings:mtree_path_prefix"),
        providers = [BuildSettingInfo],
    ),
    "_mtree_options": attr.label(
        default = Label("//img/settings:mtree_options"),
        providers = [BuildSettingInfo],
    ),
    "_mtree_layer_layout": attr.label(
        default = Label("//img/settings:mtree_layer_layout"),
        providers = [BuildSettingInfo],
    ),
}

def _platform_struct(manifest_info):
    return struct(
        os = manifest_info.os,
        architecture = manifest_info.architecture,
        variant = manifest_info.variant,
    )

def _layer_mtree(ctx, name_prefix, layer, index):
    """The mtree File for one layer, rendering one on the fly if needed.

    Reuses the layer's own `mtree` when it carries one; otherwise renders one
    from its tar blob or compact stream (mirroring `image_layer_mtrees` in
    //img/private/common:layer_helper.bzl). Returns None when neither is
    available.
    """
    mtree_file = getattr(layer, "mtree", None)
    if mtree_file != None:
        return mtree_file
    blob = getattr(layer, "blob", None)
    if blob != None and media_type_is_tar(getattr(layer, "media_type", None)):
        return build_layer_mtree(ctx, "{}_layer_{}".format(name_prefix, index), tar_blob = blob)
    compact_stream = getattr(layer, "compact_stream", None)
    if compact_stream != None:
        return build_layer_mtree(ctx, "{}_layer_{}".format(name_prefix, index), compact_stream = compact_stream)
    return None

def _manifest_image(ctx, name_prefix, manifest_info):
    """Spec entry + runfiles files for one rules_img manifest (ImageManifestInfo).

    One spec entry per layer, in layer order: its mtree (rendered on the fly
    for blob-bearing layers that carry none), a `non_tar` marker for layers
    that contribute no filesystem entries, or a `missing` marker for tar
    layers without an mtree (e.g. shallow base-image layers) -- the test
    binary fails on those with an "incomplete layer metadata" error rather
    than silently passing on a subset of the image. Each layer's metadata
    file rides along so waste can be attributed to the producing target from
    the layer history where possible.

    Returns (image_struct, files_list).
    """
    layers = getattr(manifest_info, "layers", None)
    if layers == None:
        fail("image_size_test: the `image` provider does not expose its layers, " +
             "so per-layer wasted bytes cannot be computed")
    layer_specs = []
    files = []
    for i, layer in enumerate(layers):
        spec = {}
        metadata = getattr(layer, "metadata", None)
        if metadata != None:
            spec["metadata"] = launcher.to_rlocation_path(metadata)
            files.append(metadata)
        mtree_file = _layer_mtree(ctx, name_prefix, layer, i)
        if mtree_file != None:
            spec["mtree"] = launcher.to_rlocation_path(mtree_file)
            files.append(mtree_file)
        elif not media_type_is_tar(getattr(layer, "media_type", None)):
            spec["non_tar"] = True
        else:
            spec["missing"] = True
        layer_specs.append(struct(**spec))
    image = struct(
        platform = _platform_struct(manifest_info),
        layers = layer_specs,
    )
    return image, files

def _image_size_test_aspect_impl(target, ctx):
    if ImageManifestInfo in target:
        image, runfiles_files = _manifest_image(ctx, "{}.sizetest".format(target.label.name), target[ImageManifestInfo])
        images = [image]
    elif ImageIndexInfo in target:
        images = []
        runfiles_files = []
        for i, manifest_info in enumerate(target[ImageIndexInfo].manifests):
            image, files = _manifest_image(ctx, "{}.sizetest_{}".format(target.label.name, i), manifest_info)
            images.append(image)
            runfiles_files.extend(files)
    else:
        fail("image_size_test: `image` must provide ImageManifestInfo or " +
             "ImageIndexInfo (rules_img image_manifest/image_index), since wasted " +
             "bytes are computed from the per-layer mtrees. Targets that only " +
             "expose a merged mtree (`mtree` output group or an OCI image layout " +
             "directory) cannot be checked. Got: {}".format(target.label))

    spec_file = ctx.actions.declare_file("{}.image_size_spec.json".format(target.label.name))
    ctx.actions.write(spec_file, json.encode(struct(images = images)))
    return [ImageSizeTestInfo(spec = spec_file, files = depset(runfiles_files))]

_image_size_test_aspect = aspect(
    implementation = _image_size_test_aspect_impl,
    attr_aspects = [],  # inspect the `image` target itself; do not propagate to its deps.
    attrs = _ASPECT_MTREE_ATTRS,
    toolchains = TOOLCHAINS,
    provides = [ImageSizeTestInfo],
)

def _image_size_test_impl(ctx):
    for attr_name in ("max_wasted_bytes", "max_duplicated_bytes"):
        if getattr(ctx.attr, attr_name) < -1:
            fail("image_size_test: `{}` must be -1 (unset) or a non-negative byte budget".format(attr_name))
    if ctx.attr.max_wasted_bytes < 0 and ctx.attr.max_duplicated_bytes < 0:
        fail("image_size_test: set at least one of `max_wasted_bytes` or `max_duplicated_bytes` -- " +
             "with neither budget set the test would never fail")

    input = ctx.attr.image[ImageSizeTestInfo]
    deploy_tool_info = ctx.attr._deploy_tool[DeployToolInfo]

    request = ctx.actions.declare_file(ctx.label.name + ".request.json")
    ctx.actions.write(request, json.encode(struct(
        spec = launcher.to_rlocation_path(input.spec),
        max_wasted_bytes = ctx.attr.max_wasted_bytes if ctx.attr.max_wasted_bytes >= 0 else None,
        max_duplicated_bytes = ctx.attr.max_duplicated_bytes if ctx.attr.max_duplicated_bytes >= 0 else None,
    )))

    stub = ctx.actions.declare_file(ctx.label.name + ".exe")
    embedded_args, transformed_args = launcher.args_from_entrypoint(executable_file = deploy_tool_info.img_deploy_exe)
    embedded_args.extend(["image-size-test", "--request"])
    embedded_args, transformed_args = launcher.append_runfile(
        file = request,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
    )
    launcher.compile_stub(
        ctx = ctx,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
        output_file = stub,
        template_file = deploy_tool_info.launcher_template,
    )

    runfiles = ctx.runfiles(
        files = [deploy_tool_info.img_deploy_exe, request, input.spec],
        transitive_files = input.files,
    )
    return [DefaultInfo(executable = stub, runfiles = runfiles)]

image_size_test = rule(
    implementation = _image_size_test_impl,
    doc = """Budgets the wasted and duplicated bytes an image ships across its layers.

`image_size_test` replays the image's per-layer mtrees in layer order -- like
`image_structure_test`, it never materializes layer blobs -- and fails when a
budget is exceeded:

- **Wasted bytes** (`max_wasted_bytes`): file bytes shipped in a layer that a
  later layer shadows with an overwrite, a whiteout (`.wh.<name>`), or an
  opaque whiteout (`.wh..wh..opq`). They are pulled with every image but are
  unreachable in the final filesystem.
- **Duplicated bytes** (`max_duplicated_bytes`): identical file content (by
  `sha256digest`) live at multiple paths, counted once per extra copy.
  Hardlinks (`nlink > 1`) ship their content once and symlinks carry no file
  bytes, so neither counts.

Sizes are the uncompressed tar entry sizes recorded in the mtree. At least one
budget must be set. On failure the test lists the largest offenders, each
attributed to the layer that wrote the bytes and the layer that shadowed them
(with the producing target where the layer history records it).

The `image` must be a rules_img `image_manifest` or `image_index`; for an
index, every single-platform child image is checked. If any layer contributes
no mtree (e.g. a shallow base-image layer whose blob is never fetched), the
test fails with an "incomplete layer metadata" error rather than silently
passing on a subset of the image.

Example:

```python
load("@rules_img//img:test.bzl", "image_size_test")

image_size_test(
    name = "app_size_test",
    image = ":app",
    max_wasted_bytes = 10_000_000,
)
```
""",
    attrs = {
        "image": attr.label(
            doc = "Image to check: a rules_img image_manifest or image_index.",
            mandatory = True,
            aspects = [_image_size_test_aspect],
        ),
        "max_wasted_bytes": attr.int(
            doc = "Budget for wasted bytes (shipped file bytes shadowed by a later layer). -1 (the default) leaves wasted bytes unchecked; the measured value is still reported.",
            default = -1,
        ),
        "max_duplicated_bytes": attr.int(
            doc = "Budget for duplicated bytes (identical file content live at multiple paths, counted once per extra copy). -1 (the default) leaves duplicated bytes unchecked; the measured value is still reported.",
            default = -1,
        ),
        "_deploy_tool": attr.label(
            default = Label("//img/deploy_tool/for_host"),
            providers = [DeployToolInfo],
        ),
    },
    test = True,
    toolchains = [launcher.finalizer_toolchain_type] + TOOLCHAINS,
)
