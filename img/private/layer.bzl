"""Layer rule for building layers in a container image."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/providers:layer_info.bzl", "LayerInfo")

def _file_type(f):
    type = "f"  # regular file
    if f.is_directory:
        type = "d"
    return type

def _files_arg(f):
    type = _file_type(f)
    return "{}{}".format(type, f.path)

def _to_short_path_pair(f):
    repo = f.owner.repo_name
    if repo == "":
        repo = "_main"
    type = _file_type(f)
    return "{}/{}\0{}{}".format(repo, f.short_path, type, f.path)

def _root_symlinks_arg(x):
    type = _file_type(x.target_file)
    return "{}\0{}{}".format(x.path, type, x.target_file.path)

def _symlinks_arg(x):
    type = _file_type(x.target_file)
    return "{}\0{}{}_main/{}".format(x.path, type, x.target_file.path)

def _symlink_tuple_to_arg(pair):
    source = pair[0]
    dest = pair[1]
    if source.startswith("/"):
        source = source[1:]
    return "{}\0{}".format(source, dest)

def _image_layer_impl(ctx):
    compression = ctx.attr.compress
    if compression == "auto":
        compression = ctx.attr._default_compression[BuildSettingInfo].value

    estargz = ctx.attr.estargz
    if estargz == "auto":
        estargz = ctx.attr._default_estargz[BuildSettingInfo].value
    estargz_enabled = estargz == "enabled"

    if compression == "gzip":
        out_ext = ".tgz"
        media_type = "application/vnd.oci.image.layer.v1.tar+gzip"
    elif compression == "zstd":
        out_ext = ".tar.zst"
        media_type = "application/vnd.oci.image.layer.v1.tar+zstd"
    else:
        fail("Unsupported compression: {}".format(compression))

    out = ctx.actions.declare_file(ctx.attr.name + out_ext)
    metadata_out = ctx.actions.declare_file(ctx.attr.name + "_metadata.json")
    args = ["layer", "--name", str(ctx.label), "--metadata", metadata_out.path, "--format", compression]
    if estargz_enabled:
        args.append("--estargz")
    for key, value in ctx.attr.annotations.items():
        args.extend(["--annotation", "{}={}".format(key, value)])
    if ctx.attr.default_metadata:
        args.extend(["--default-metadata", ctx.attr.default_metadata])
    for path, metadata in ctx.attr.file_metadata.items():
        path = path.removeprefix("/")  # the "/" is not included in the tar file.
        args.extend(["--file-metadata", "{}={}".format(path, metadata)])
    files_args = ctx.actions.args()
    files_args.set_param_file_format("multiline")
    files_args.use_param_file("--add-from-file=%s", use_always = True)

    inputs = []

    for (path_in_image, files) in ctx.attr.srcs.items():
        path_in_image = path_in_image.removeprefix("/")  # the "/" is not included in the tar file.
        default_info = files[DefaultInfo]
        files_to_run = default_info.files_to_run
        executable = None
        runfiles = None
        inputs.append(default_info.files)
        if files_to_run != None and files_to_run.executable != None and not files_to_run.executable.is_source:
            # This is an executable.
            # Add the executable with the runfiles tree, but ignore any other files.
            executable = files_to_run.executable
            runfiles = default_info.default_runfiles
            args.append("--executable={}={}".format(path_in_image, executable.path))
            executable_runfiles_args = ctx.actions.args()
            executable_runfiles_args.set_param_file_format("multiline")
            executable_runfiles_args.use_param_file("--runfiles={}=%s".format(executable.path), use_always = True)
            executable_runfiles_args.add_all(runfiles.files, map_each = _to_short_path_pair, expand_directories = False)
            executable_runfiles_args.add_all(runfiles.symlinks, map_each = _symlinks_arg)
            executable_runfiles_args.add_all(runfiles.root_symlinks, map_each = _root_symlinks_arg)
            args.append(executable_runfiles_args)
            inputs.append(runfiles.files)
            inputs.append(runfiles.symlinks)
            inputs.append(runfiles.root_symlinks)
            continue

        # This isn't an executable.
        # Let's add all files instead.
        if default_info.files == None:
            fail("Expected {} ({}) to contain an executable or files, got None".format(path_in_image, files))
        files_args.add_all(default_info.files, map_each = _files_arg, format_each = "{}\0%s".format(path_in_image), expand_directories = False)

    if len(ctx.attr.symlinks) > 0:
        symlink_args = ctx.actions.args()
        symlink_args.set_param_file_format("multiline")
        symlink_args.use_param_file("--symlinks-from-file=%s", use_always = True)
        symlink_args.add_all(ctx.attr.symlinks.items(), map_each = _symlink_tuple_to_arg)
        args.append(symlink_args)
    args.append(files_args)
    args.append(out.path)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        outputs = [out, metadata_out],
        inputs = depset(transitive = inputs),
        executable = img_toolchain_info.tool_exe,
        arguments = args,
        mnemonic = "LayerTar",
    )
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
            estargz = estargz_enabled,
        ),
    ]

image_layer = rule(
    implementation = _image_layer_impl,
    attrs = {
        "srcs": attr.string_keyed_label_dict(
            doc = "Files (including regular files, executables, and TreeArtifacts) that should be added to the layer.",
            allow_files = True,
        ),
        "symlinks": attr.string_dict(
            doc = "Symlinks that should be added to the layer.",
        ),
        "compress": attr.string(
            default = "auto",
            values = ["auto", "gzip", "zstd"],
            doc = """Compression algorithm to use. If set to 'auto', uses the global default compression setting.""",
        ),
        "estargz": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = """Whether to use estargz format. If set to 'auto', uses the global default estargz setting.
When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.""",
        ),
        "annotations": attr.string_dict(
            default = {},
            doc = """Annotations to add to the layer metadata as key-value pairs.""",
        ),
        "default_metadata": attr.string(
            default = "",
            doc = """JSON-encoded default metadata to apply to all files in the layer.
Can include fields like mode, uid, gid, uname, gname, mtime, and pax_records.""",
        ),
        "file_metadata": attr.string_dict(
            default = {},
            doc = """Per-file metadata overrides as a dict mapping file paths to JSON-encoded metadata.
The path should match the path in the image (the key in srcs attribute).
Metadata specified here overrides any defaults from default_metadata.""",
        ),
        "_default_compression": attr.label(
            default = Label("//img/settings:compress"),
            providers = [BuildSettingInfo],
        ),
        "_default_estargz": attr.label(
            default = Label("//img/settings:estargz"),
            providers = [BuildSettingInfo],
        ),
    },
    toolchains = TOOLCHAINS,
    provides = [LayerInfo],
)
