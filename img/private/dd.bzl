"""Rule that writes the bytes of a disk image or a layer, like dd."""

load("@hermetic_launcher//launcher:lib.bzl", "launcher")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:default_deploy_tool.bzl", "default_deploy_tool")
load("//img/private/common:transitions.bzl", "compact_image_transition")
load("//img/private/providers:deploy_tool_info.bzl", "DeployToolInfo")
load("//img/private/providers:disk_image_info.bzl", "DiskImageInfo")
load("//img/private/providers:layers_info.bzl", "LayersInfo")
load("//img/private/providers:load_settings_info.bzl", "LoadSettingsInfo")

def _blob_descriptor(ctx, blob):
    """Describes a file by its digest and size, so that it can be fetched by digest.

    The hash command is a worker that can take the digest from Bazel, so the file
    is not read; executed remotely, the action does not make the file available
    locally.

    Args:
        ctx: rule context.
        blob: the File to describe.

    Returns:
        a JSON File with the "digest" and the "size" of the blob.
    """
    descriptor = ctx.actions.declare_file(ctx.label.name + ".blob.json")
    args = ctx.actions.args()
    args.add("--digest=sha256")
    args.add("--encoding=layer-metadata")
    args.add("--digest-mode", "digest")
    args.add("--media-type", "application/octet-stream")
    args.add(blob)
    args.add(descriptor)
    args.set_param_file_format("multiline")
    args.use_param_file("@%s", use_always = True)
    ctx.actions.run(
        inputs = [blob],
        outputs = [descriptor],
        executable = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo.tool_exe,
        arguments = ["hash", args],
        mnemonic = "BlobDescriptor",
        execution_requirements = {
            "requires-worker-protocol": "json",
            "supports-workers": "1",
            "supports-multiplex-workers": "1",
            "supports-multiplex-sandboxing": "1",
            "supports-worker-cancellation": "1",
            "supports-path-mapping": "1",
        },
    )
    return descriptor

def _source(ctx, strategy):
    """Finds what the tool reads, and the content-addressed directories it needs.

    Args:
        ctx: rule context.
        strategy: "eager" or "lazy".

    Returns:
        tuple of (File, whether it is a blob descriptor, list of content-addressed
        directories). Otherwise, the File is the image or a stream.
    """
    target = ctx.attr.image

    # An attribute with a transition is a list of one target.
    if type(target) == "list":
        target = target[0]

    # What is only a file is eager, or lazy through a descriptor with its digest
    # (the tool then fetches the blob like the blobs a stream references).
    if DiskImageInfo in target:
        info = target[DiskImageInfo]
        if info.extent_stream == None:
            if strategy == "lazy":
                return _blob_descriptor(ctx, info.image), True, []
            return info.image, False, []

        # The extent stream of a disk image references files by digest, so they can
        # be fetched from the remote cache when they are needed instead of being
        # part of the runfiles.
        cas_dirs = info.cas_dirs.to_list() if strategy == "eager" else []
        return info.extent_stream, False, cas_dirs

    layers = target[LayersInfo].layers
    if len(layers) != 1:
        fail("{}: image must be a layer target that produces exactly one layer, but {} produces {}.".format(ctx.label, target.label, len(layers)))
    layer = layers[0]
    if layer.blob == None and layer.compact_stream == None:
        fail("{}: the layer {} has neither a blob nor a compact stream.".format(ctx.label, target.label))

    # A layer built as a compact stream references the files it was made of.
    # Lazily, they are fetched when they are written; eagerly, the layer file is
    # used if there is one, and the files are part of the runfiles otherwise.
    if layer.compact_stream != None and (strategy == "lazy" or layer.blob == None):
        cas_dirs = [layer.layer_input_files_cas] if strategy == "eager" and layer.layer_input_files_cas != None else []
        return layer.compact_stream, False, cas_dirs

    # The metadata of a layer has the digest and the size of the layer file.
    if strategy == "lazy" and layer.metadata != None:
        return layer.metadata, True, []
    return layer.blob, False, []

def _image_dd_impl(ctx):
    load_settings = ctx.attr._load_settings[LoadSettingsInfo]
    strategy = ctx.attr.strategy
    if strategy == "auto":
        strategy = load_settings.strategy
    source, is_descriptor, cas_dirs = _source(ctx, strategy)

    deploy_tool_info = ctx.attr.deploy_tool[DeployToolInfo] if ctx.attr.deploy_tool != None else ctx.attr._deploy_tool[DeployToolInfo]
    embedded_args, transformed_args = launcher.args_from_entrypoint(executable_file = deploy_tool_info.img_deploy_exe)
    embedded_args.extend(["dd", "--blob-descriptor" if is_descriptor else "--if"])
    embedded_args, transformed_args = launcher.append_runfile(
        file = source,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
    )
    runfiles_files = [deploy_tool_info.img_deploy_exe, source] + cas_dirs

    # The content-addressed directories are passed in a file, not as a --cas-dir
    # flag each: a launcher embeds only a handful of arguments, and a disk image
    # has a directory per partition. The tool resolves the runfiles paths in it.
    if len(cas_dirs) > 0:
        cas_dir_manifest = ctx.actions.declare_file(ctx.label.name + ".cas_dirs.txt")
        ctx.actions.write(
            output = cas_dir_manifest,
            content = "".join([launcher.to_rlocation_path(cas_dir) + "\n" for cas_dir in cas_dirs]),
        )
        runfiles_files.append(cas_dir_manifest)
        embedded_args, transformed_args = launcher.append_embedded_arg(
            arg = "--cas-dir-manifest",
            embedded_args = embedded_args,
            transformed_args = transformed_args,
        )
        embedded_args, transformed_args = launcher.append_runfile(
            file = cas_dir_manifest,
            embedded_args = embedded_args,
            transformed_args = transformed_args,
        )

    writer = ctx.actions.declare_file(ctx.label.name + ".exe")
    launcher.compile_stub(
        ctx = ctx,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
        output_file = writer,
        template_file = deploy_tool_info.launcher_template,
    )

    environment = {
        "IMG_REAPI_ENDPOINT": load_settings.remote_cache,
        "IMG_REAPI_INSTANCE_NAME": load_settings.remote_instance_name,
        "IMG_CREDENTIAL_HELPER": load_settings.credential_helper,
        "IMG_CREDENTIAL_HELPER_REMOTE_CACHE": load_settings.credential_helper_remote_cache,
    }
    inherited_environment = [
        "IMG_REAPI_ENDPOINT",
        "IMG_REAPI_INSTANCE_NAME",
        "IMG_CREDENTIAL_HELPER",
        "IMG_CREDENTIAL_HELPER_REMOTE_CACHE",
        "IMG_DISK_CACHE",
        "IMG_CAS_CACHE",
        "IMG_CAS_CACHE_DIR",
        "IMG_CAS_CACHE_MAX_SIZE",
        "IMG_AUTH_DEBUG",
    ]
    return [
        DefaultInfo(
            files = depset([writer]),
            executable = writer,
            runfiles = ctx.runfiles(files = runfiles_files),
        ),
        RunEnvironmentInfo(
            environment = environment,
            inherited_environment = inherited_environment,
        ),
    ]

image_dd = rule(
    implementation = _image_dd_impl,
    doc = """(Experimental) Writes the bytes of a disk image or a layer, like `dd`.

The target is executable: `bazel run //:write_disk -- of=/dev/sdX bs=4M status=progress`
writes (or reconstructs) the bytes of the disk image or the layer to a block device,
a file or, without `of=`, to stdout. A layer is what `image_layer` produces,
for example a filesystem image (`format = "ext4"`, `"fat32"`, `"erofs"`) or an
initramfs (`format = "cpio"`) that goes onto a partition or a boot medium. It takes the operands of `dd(1)` as `name=value` or as
`--name=value`:

| Operand | |
|---------|-|
| `of=FILE` | where to write: a file, a block device, or `-` for stdout (default) |
| `bs=BYTES` | block size for writing and for detecting holes (default 1MiB) |
| `skip=N`, `count=N` | skip N input blocks, copy only N input blocks (`iflag=skip_bytes,count_bytes`: in bytes) |
| `seek=N` | skip N output blocks, leaving them as they are (`oflag=seek_bytes`: in bytes) |
| `status=none\\|noxfer\\|progress` | what to print to stderr (default: a summary at the end) |
| `conv=sparse` | seek over holes and blocks of zeros instead of writing them |
| `conv=nosparse` | write zeros |
| `conv=notrunc`, `conv=fsync` | don't truncate the output file; synchronize before finishing |

Holes (free space of the disk, and blocks of zeros) are not written to an output
file that is created or truncated, which leaves the file sparse. For an existing
device, or with `conv=notrunc`, the zeros are written, unless `conv=sparse` promises
that the output already reads as zeros (a new or erased device). Writing to a
device is destructive: the target does not ask.

If `image` is a `disk_image` built as an (experimental) extent stream
(`extent_stream = "enabled"`), or a layer built as a compact stream
(`experimental_compact_layers`), the target reconstructs the bytes from it. With
`strategy = "lazy"` (or `--@rules_img//img/settings:load_strategy=lazy`), only the extent
stream or compact stream is part of the runfiles: the streams and blobs it
references are fetched by digest when they are needed, from the Bazel disk cache
(`IMG_DISK_CACHE`) or the remote cache (`IMG_REAPI_ENDPOINT`, see
`--@rules_img//img/settings:remote_cache`), and only the parts that `skip` and `count`
select are read. With `eager`, the content-addressed directories are part of the
runfiles and are downloaded by Bazel before the target runs (a layer with a layer
file uses the file instead).

A disk image that is a raw image, and a layer that is only a file, are lazy the same
way: the runfiles only have a small description of the file (its digest and size,
which a layer has already), and the bytes are fetched by digest, from the same
sources, when the target runs. With `eager`, the file is part of the runfiles.

With `compact = "enabled"`, `image` is built as compact streams and an extent stream all
the way, whatever the command line says (see `compact`).

Operands that should always be passed can be set with the `args` attribute
(`args = ["bs=4M", "status=progress"]`); the ones on the command line come after them.

The image is written by the `img dd` command.
""",
    attrs = {
        "image": attr.label(
            doc = "The `disk_image` or the layer (a target of `image_layer` or `layer_from_tar` that produces one layer) to write.",
            mandatory = True,
            providers = [[DiskImageInfo], [LayersInfo]],
            cfg = compact_image_transition,
        ),
        "compact": attr.string(
            doc = """(Experimental) Whether to build `image` as compact streams and extent streams all the way.

- **`auto`** (default): `image` is built with the settings of the command line.
- **`enabled`**: layers are built as compact streams (`experimental_compact_layers`, without materializing the layer blob) and a `disk_image` as an extent stream (`disk_image_extent_stream`). The image then only references the files it is made of, which are fetched by digest when the target runs (see `strategy`), instead of being built in full.
- **`disabled`**: layers and disk images are built in full.

This changes the configuration of `image` and everything it depends on, so it is built separately from the same targets built with another value.""",
            default = "auto",
            values = ["auto", "enabled", "disabled"],
        ),
        "strategy": attr.string(
            doc = """How the bytes are made available.

- **`auto`** (default): uses the global `--@rules_img//img/settings:load_strategy`.
- **`eager`**: the file, or the files that a stream references, are part of the runfiles.
- **`lazy`**: only a stream or a description of the file (digest and size) is. The rest is fetched by digest when it is needed.
""",
            default = "auto",
            values = ["auto", "eager", "lazy"],
        ),
        "tool_cfg": attr.string(
            doc = """**Experimental**: Configuration of the `img` executable. By default it matches the host platform; `"target"` makes it match the target platform.""",
            default = "host",
            values = ["host", "target"],
        ),
        "deploy_tool": attr.label(
            doc = "Optional label of a deploy tool target providing `DeployToolInfo` (created with `img_deploy_tool` from `@rules_img//img:deploy_tool.bzl`). When set, overrides `tool_cfg`.",
            mandatory = False,
            providers = [DeployToolInfo],
        ),
        "_deploy_tool": attr.label(
            default = default_deploy_tool,
            providers = [DeployToolInfo],
        ),
        "_load_settings": attr.label(
            default = Label("//img/private/settings:load"),
            providers = [LoadSettingsInfo],
        ),
    },
    executable = True,
    toolchains = [launcher.finalizer_toolchain_type] + TOOLCHAINS,
)
