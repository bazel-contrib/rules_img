"""Records which Bazel targets a deploy was built from, for signer plugins that ask for it.

`img_build_info_aspect` walks the image graph below a deploy rule (`image_push`,
`multi_deploy`) and collects, per target, its label, its configuration and the
file that holds its digest. Collecting is only analysis-time work on depsets. The
action that turns the collection into a file, `build_info_file`, is registered
only when a signer plugin opted in (`signing_config(build_info = True)`), so deploys
that are not signed that way pay nothing at execution time.
"""

load("//img/private:sign_settings.bzl", "BUILD_INFO_RUNFILES_PATH")
load("//img/private/common:build.bzl", "TOOLCHAIN")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:layers_info.bzl", "LayersInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")
load("//img/private/providers:single_layer_info.bzl", "SingleLayerInfo")

ImgBuildInfo = provider(
    doc = "The image, layer and pulled base image targets below a target (see `img_build_info_aspect`).",
    fields = {
        "entries": "depset of struct(kind, label, config, file, base, platform, pull): one per target that provides an image, an image index, a layer or a pulled image. `file` is the manifest, index or layer metadata file (None for pull-only entries).",
    },
)

def _configuration(ctx):
    # bazel-out/<configuration>/bin
    parts = ctx.bin_dir.path.split("/")
    return parts[1] if len(parts) > 1 else ""

def _build_info_aspect_impl(target, ctx):
    transitive = []
    for name in ["base", "layers", "manifests", "image", "operations"]:
        deps = getattr(ctx.rule.attr, name, None)
        if deps == None:
            continue
        if type(deps) != "list":
            deps = [deps]
        for dep in deps:
            if ImgBuildInfo in dep:
                transitive.append(dep[ImgBuildInfo].entries)

    label = str(target.label)
    config = _configuration(ctx)
    direct = []
    base = getattr(ctx.rule.attr, "base", None)
    base_label = str(base.label) if base != None and type(base) != "list" else None
    if ImageManifestInfo in target:
        image = target[ImageManifestInfo]
        platform = struct(os = image.os, architecture = image.architecture, variant = image.variant)
        direct.append(struct(kind = "image", label = label, config = config, file = image.manifest, base = base_label, pull = None, platform = platform))
    if ImageIndexInfo in target:
        direct.append(struct(kind = "index", label = label, config = config, file = target[ImageIndexInfo].index, base = base_label, pull = None, platform = None))
    layers = []
    if LayersInfo in target:
        layers = target[LayersInfo].layers
    elif SingleLayerInfo in target:
        layers = [target[SingleLayerInfo]]
    for layer in layers:
        direct.append(struct(kind = "layer", label = label, config = config, file = layer.metadata, base = None, pull = None, platform = None))
    if PullInfo in target:
        pull = target[PullInfo]
        direct.append(struct(
            kind = "pull",
            label = label,
            config = config,
            file = None,
            base = None,
            platform = None,
            pull = struct(registries = pull.registries, repository = pull.repository, tag = pull.tag, digest = pull.digest),
        ))
    return [ImgBuildInfo(entries = depset(direct, transitive = transitive))]

img_build_info_aspect = aspect(
    implementation = _build_info_aspect_impl,
    attr_aspects = ["base", "layers", "manifests", "image", "operations"],
    doc = "Collects the image, layer and pulled base image targets below a deploy rule. See `build_info_file`.",
)

def _entry_line(entry):
    record = {"kind": entry.kind, "label": entry.label, "config": entry.config}
    if entry.file != None:
        record["file"] = entry.file.path
    if entry.base != None:
        record["base"] = entry.base
    if entry.platform != None:
        record["platform"] = {"os": entry.platform.os, "architecture": entry.platform.architecture, "variant": entry.platform.variant}
    if entry.pull != None:
        record["pull"] = {
            "registries": entry.pull.registries,
            "repository": entry.pull.repository,
            "tag": entry.pull.tag,
            "digest": entry.pull.digest,
        }
    return json.encode(record)

def wants_build_info(steps):
    """Whether any of the signing steps asked for the build info."""
    return any([getattr(step, "build_info", False) for step in steps])

def build_info_file(ctx, *, deps):
    """Declares the build info file of a deploy rule.

    Args:
        ctx: The deploy rule's context.
        deps: Targets (with `img_build_info_aspect` applied) the deploy is built from.

    Returns:
        The file, to be shipped at `BUILD_INFO_RUNFILES_PATH` in the deploy binary's runfiles.
    """
    entries = depset(transitive = [dep[ImgBuildInfo].entries for dep in deps if ImgBuildInfo in dep])
    files = depset([entry.file for entry in entries.to_list() if entry.file != None])

    args = ctx.actions.args()
    args.add("--label", str(ctx.label))
    args.add("--configuration", _configuration(ctx))
    args.add_all(entries, map_each = _entry_line, before_each = "--entry")
    args.set_param_file_format("multiline")
    args.use_param_file("@%s", use_always = True)

    out = ctx.actions.declare_file(ctx.label.name + ".build_info.json")
    args.add("--output", out)
    ctx.actions.run(
        inputs = files,
        outputs = [out],
        executable = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo.tool_exe,
        arguments = ["build-info", args],
        mnemonic = "ImgBuildInfo",
        progress_message = "Recording build info of %{label}",
    )
    return out

def add_build_info_symlink(root_symlinks, build_info):
    """Ships the build info file where `img deploy` looks for it."""
    root_symlinks[BUILD_INFO_RUNFILES_PATH] = build_info
