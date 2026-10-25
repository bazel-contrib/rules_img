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
load(
    "//img/private/common:layer_helper.bzl",
    "LAYER_MTREE_ATTRS",
    "layer_mtree_or_none",
    "media_type_ships_no_filesystem",
)
load("//img/private/providers:deploy_tool_info.bzl", "DeployToolInfo")
load("//img/private/providers:image_size_test_info.bzl", "ImageSizeTestInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")

# mtree keywords the replay cannot work without: `type` to tell files from
# directories, `size` to count bytes, `nlink` to recognize a hardlink group as
# the single shipped copy it is. `sha256` is only needed for duplicated bytes,
# and its absence is reported per-file by the test binary (which is the only
# place that knows whether a duplicate budget was set).
_REQUIRED_MTREE_OPTIONS = ["type", "size", "nlink"]

def _check_mtree_settings(ctx):
    """Fails unless the //img/settings:mtree_* flags produce analyzable mtrees.

    The replay reads the per-layer mtrees as literal tar listings: the
    "oci_layer_filesystem_applied_changeset" layer layout consumes the whiteout
    markers it needs (so every deletion would silently stop counting as waste),
    and a narrowed `mtree_options` drops keywords it reads (so entries would
    silently stop counting at all). Both would turn a budget into a test that
    passes for the wrong reason, so they are rejected up front.
    """
    layout = ctx.attr._mtree_layer_layout[BuildSettingInfo].value
    if layout != "tar":
        fail("image_size_test requires --//img/settings:mtree_layer_layout=tar, got {}. ".format(layout) +
             "The \"{}\" layout applies each layer's whiteouts while rendering it, ".format(layout) +
             "so the markers that make a deletion count as wasted bytes are already gone.")
    options = [field.strip() for field in ctx.attr._mtree_options[BuildSettingInfo].value.split(",")]
    missing = [field for field in _REQUIRED_MTREE_OPTIONS if field not in options]
    if missing:
        fail("image_size_test requires --//img/settings:mtree_options to include {}, ".format(", ".join(missing)) +
             "got \"{}\". Without them the per-layer mtrees do not carry the ".format(",".join(options)) +
             "file sizes, types and hardlink counts the byte accounting reads.")

def _platform_struct(manifest_info):
    return struct(
        os = manifest_info.os,
        architecture = manifest_info.architecture,
        variant = manifest_info.variant,
    )

def _unique_runfile(ctx, name, file):
    """A symlink to `file` at a runfiles path unique to this spec entry.

    An `image_index`'s per-platform child images come from a split transition,
    which leaves their `short_path` (and therefore their rlocation path)
    identical. Writing them into the test runfiles as they are would collapse
    every platform onto whichever copy won, so each platform's files are
    symlinked under a per-platform prefix first.
    """
    out = ctx.actions.declare_file(name)
    ctx.actions.symlink(output = out, target_file = file)
    return out

def _manifest_image(ctx, name_prefix, manifest_info, disambiguate_runfiles = False):
    """Spec entry + runfiles files for one rules_img manifest (ImageManifestInfo).

    One spec entry per layer, in layer order: its mtree (rendered on the fly for
    blob-bearing layers that carry none), a `non_tar` marker for layers whose
    media type states they contribute no filesystem entries, or a `missing`
    marker for any other layer without an mtree (e.g. a shallow base-image
    layer, or a layer whose media type is unknown) -- the test binary fails on
    those with an "incomplete layer metadata" error rather than silently passing
    on a subset of the image. Each layer's metadata file rides along so waste
    can be attributed to the producing target from the layer history where
    possible.

    Returns (image_struct, files_list).
    """
    layers = getattr(manifest_info, "layers", None)
    if layers == None:
        fail("image_size_test: the `image` provider does not expose its layers, " +
             "so per-layer wasted bytes cannot be computed. Got: {}".format(ctx.label))
    layer_specs = []
    files = []
    for i, layer in enumerate(layers):
        spec = {}
        metadata = getattr(layer, "metadata", None)
        if metadata != None:
            if disambiguate_runfiles:
                metadata = _unique_runfile(ctx, "{}/layer_{}.metadata.json".format(name_prefix, i), metadata)
            spec["metadata"] = launcher.to_rlocation_path(metadata)
            files.append(metadata)
        mtree_file = layer_mtree_or_none(ctx, layer, "{}_layer_{}".format(name_prefix, i))
        if mtree_file != None:
            if disambiguate_runfiles:
                mtree_file = _unique_runfile(ctx, "{}/layer_{}.mtree".format(name_prefix, i), mtree_file)
            spec["mtree"] = launcher.to_rlocation_path(mtree_file)
            files.append(mtree_file)
        elif media_type_ships_no_filesystem(getattr(layer, "media_type", None)):
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
    _check_mtree_settings(ctx)
    if ImageManifestInfo in target:
        image, runfiles_files = _manifest_image(ctx, "{}.sizetest".format(target.label.name), target[ImageManifestInfo])
        images = [image]
    elif ImageIndexInfo in target:
        images = []
        runfiles_files = []
        for i, manifest_info in enumerate(target[ImageIndexInfo].manifests):
            image, files = _manifest_image(
                ctx,
                "{}.sizetest_{}".format(target.label.name, i),
                manifest_info,
                # Children of an index differ only by configuration, so their
                # runfiles paths have to be made distinct.
                disambiguate_runfiles = True,
            )
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
    attrs = LAYER_MTREE_ATTRS,
    toolchains = TOOLCHAINS,
    provides = [ImageSizeTestInfo],
)

# Byte-count suffixes accepted in a budget, as powers of two (matching
# `img deploy`'s byte-size flags and Bazel's conventions elsewhere).
_BYTE_UNITS = {
    "": 1,
    "b": 1,
    "k": 1024,
    "kb": 1024,
    "kib": 1024,
    "m": 1024 * 1024,
    "mb": 1024 * 1024,
    "mib": 1024 * 1024,
    "g": 1024 * 1024 * 1024,
    "gb": 1024 * 1024 * 1024,
    "gib": 1024 * 1024 * 1024,
    "t": 1024 * 1024 * 1024 * 1024,
    "tb": 1024 * 1024 * 1024 * 1024,
    "tib": 1024 * 1024 * 1024 * 1024,
}

def _parse_budget(attr_name, raw):
    """Parses a byte budget attribute into an int, or None when unset.

    The budgets are string attributes rather than `attr.int` because Bazel
    integer attributes are limited to the signed 32-bit range, which cannot
    express a budget above ~2.1 GB -- an ordinary size for the large images this
    rule exists to measure.
    """
    value = raw.strip().lower()
    if value == "":
        return None
    digits = value.rstrip("kmgtib ")
    unit = value[len(digits):].strip()
    if unit not in _BYTE_UNITS or not digits.isdigit():
        fail("image_size_test: `{}` must be a byte count, optionally with a ".format(attr_name) +
             "KiB/MiB/GiB/TiB suffix (e.g. \"10000000\", \"512MiB\"); got \"{}\"".format(raw))
    return int(digits) * _BYTE_UNITS[unit]

def _image_size_test_impl(ctx):
    max_wasted_bytes = _parse_budget("max_wasted_bytes", ctx.attr.max_wasted_bytes)
    max_duplicated_bytes = _parse_budget("max_duplicated_bytes", ctx.attr.max_duplicated_bytes)
    if max_wasted_bytes == None and max_duplicated_bytes == None:
        fail("image_size_test: set at least one of `max_wasted_bytes` or `max_duplicated_bytes` -- " +
             "with neither budget set the test would never fail")

    input = ctx.attr.image[ImageSizeTestInfo]
    deploy_tool_info = ctx.attr._deploy_tool[DeployToolInfo]

    request = ctx.actions.declare_file(ctx.label.name + ".request.json")
    ctx.actions.write(request, json.encode(struct(
        spec = launcher.to_rlocation_path(input.spec),
        max_wasted_bytes = max_wasted_bytes,
        max_duplicated_bytes = max_duplicated_bytes,
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

`image_size_test` replays the image's per-layer mtrees in layer order -- the
test itself never reads a layer blob -- and fails when a budget is exceeded:

- **Wasted bytes** (`max_wasted_bytes`): file bytes shipped in a layer that a
  later layer shadows with an overwrite, a whiteout (`.wh.<name>`), or an
  opaque whiteout (`.wh..wh..opq`). They are pulled with every image but are
  unreachable in the final filesystem.
- **Duplicated bytes** (`max_duplicated_bytes`): identical file content (by
  `sha256digest`) live at multiple paths, counted once per extra copy. A
  hardlink group ships its content once however many names point at it, and
  symlinks carry no file bytes, so neither inflates the count.

Both are counted in uncompressed tar entry sizes, so they are larger than the
compressed bytes a registry reports for the same layers. At least one budget
must be set. On failure the test lists the largest offenders, each attributed to
the layer that wrote the bytes and the layer that shadowed them (with the
producing target where the layer history records it).

The `image` must be a rules_img `image_manifest` or `image_index`; for an
index, every single-platform child image is checked.

Every layer must expose its filesystem content, otherwise the test fails with an
"incomplete layer metadata" error rather than silently passing on a subset of
the image. In particular a base image pulled with the default
`layer_handling = "shallow"` never fetches its layer blobs, so it has no mtree
to replay: pull it with `layer_handling = "eager"` to make an image on top of it
measurable.

Example:

```python
load("@rules_img//img:test.bzl", "image_size_test")

image_size_test(
    name = "app_size_test",
    image = ":app",
    max_wasted_bytes = "10MiB",
)
```
""",
    attrs = {
        "image": attr.label(
            doc = "Image to check: a rules_img image_manifest or image_index.",
            mandatory = True,
            aspects = [_image_size_test_aspect],
        ),
        "max_wasted_bytes": attr.string(
            doc = """Budget for wasted bytes (shipped file bytes shadowed by a later layer).

A byte count, optionally with a `KiB`/`MiB`/`GiB`/`TiB` suffix (e.g. `"10000000"`,
`"512MiB"`). Empty (the default) leaves wasted bytes unchecked; the measured
value is still reported.""",
            default = "",
        ),
        "max_duplicated_bytes": attr.string(
            doc = """Budget for duplicated bytes (identical file content live at multiple paths, counted once per extra copy).

A byte count, optionally with a `KiB`/`MiB`/`GiB`/`TiB` suffix (e.g. `"10000000"`,
`"512MiB"`). Empty (the default) leaves duplicated bytes unchecked; the measured
value is still reported.""",
            default = "",
        ),
        "_deploy_tool": attr.label(
            default = Label("//img/deploy_tool/for_host"),
            providers = [DeployToolInfo],
        ),
    },
    test = True,
    toolchains = [launcher.finalizer_toolchain_type] + TOOLCHAINS,
)
