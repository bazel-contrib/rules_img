"""Layer rule for building layers in a container image."""

load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:layer_attrs.bzl", "layer_attrs")
load(
    "//img/private/common:tar_layer.bzl",
    "create_tar_layer",
    "files_arg",
    "get_repo_mapping_manifest",
    "resolve_layer_settings",
    "root_symlinks_arg",
    "symlinks_arg",
    "to_short_path_pair",
)
load("//img/private/providers:layers_info.bzl", "LayersInfo")

def _symlink_tuple_to_arg(pair):
    source = pair[0]
    dest = pair[1]
    if source.startswith("/"):
        source = source[1:]
    return "{}\0{}".format(source, dest)

def _image_layer_impl(ctx):
    settings = resolve_layer_settings(ctx)

    extra_args = []
    extra_inputs = []

    if ctx.attr.default_metadata:
        extra_args.extend(["--default-metadata", ctx.attr.default_metadata])
    for path, metadata in ctx.attr.file_metadata.items():
        path = path.removeprefix("/")  # the "/" is not included in the tar file.
        extra_args.extend(["--file-metadata", "{}={}".format(path, metadata)])

    files_args = ctx.actions.args()
    files_args.set_param_file_format("multiline")
    files_args.use_param_file("--add-from-file=%s", use_always = True)

    for (path_in_image, files) in ctx.attr.srcs.items():
        path_in_image = path_in_image.removeprefix("/")  # the "/" is not included in the tar file.
        default_info = files[DefaultInfo]
        files_to_run = default_info.files_to_run
        extra_inputs.append(default_info.files)
        if ctx.attr.include_runfiles and files_to_run != None and files_to_run.executable != None and not files_to_run.executable.is_source:
            # This is an executable.
            # Add the executable with the runfiles tree, but ignore any other files.
            executable = files_to_run.executable
            runfiles = default_info.default_runfiles
            extra_args.append("--executable={}={}".format(path_in_image, executable.path))
            executable_runfiles_args = ctx.actions.args()
            executable_runfiles_args.set_param_file_format("multiline")
            executable_runfiles_args.use_param_file("--runfiles={}=%s".format(executable.path), use_always = True)
            executable_runfiles_args.add_all(runfiles.files, map_each = to_short_path_pair, expand_directories = False, uniquify = True)
            executable_runfiles_args.add_all(runfiles.symlinks, map_each = symlinks_arg)
            executable_runfiles_args.add_all(runfiles.root_symlinks, map_each = root_symlinks_arg)
            extra_args.append(executable_runfiles_args)
            extra_inputs.append(runfiles.files)
            symlink_inputs = []
            symlink_inputs.extend([symlink_entry.target_file for symlink_entry in runfiles.symlinks.to_list()])
            symlink_inputs.extend([symlink_entry.target_file for symlink_entry in runfiles.root_symlinks.to_list()])
            if len(symlink_inputs) > 0:
                extra_inputs.append(depset(symlink_inputs))
            repo_mapping_manifest = get_repo_mapping_manifest(files)
            if repo_mapping_manifest != None:
                extra_inputs.append(depset([repo_mapping_manifest]))
                files_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}.repo_mapping\0%s".format(path_in_image), expand_directories = False)
                files_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}.runfiles/_repo_mapping\0%s".format(path_in_image), expand_directories = False)
            continue

        # This isn't an executable (or include_runfiles is False).
        # Let's add all files instead.
        if default_info.files == None:
            fail("Expected {} ({}) to contain an executable or files, got None".format(path_in_image, files))
        files_args.add_all(default_info.files, map_each = files_arg, format_each = "{}\0%s".format(path_in_image), expand_directories = False)

    if len(ctx.attr.symlinks) > 0:
        symlink_args = ctx.actions.args()
        symlink_args.set_param_file_format("multiline")
        symlink_args.use_param_file("--symlinks-from-file=%s", use_always = True)
        symlink_args.add_all(ctx.attr.symlinks.items(), map_each = _symlink_tuple_to_arg)
        extra_args.append(symlink_args)
    extra_args.append(files_args)

    return create_tar_layer(ctx, settings, extra_args = extra_args, extra_inputs = extra_inputs)

image_layer = rule(
    implementation = _image_layer_impl,
    doc = """Creates a container image layer from files, executables, and directories.

This rule packages files into a layer that can be used in container images. It supports:
- Adding files at specific paths in the image
- Setting file permissions and ownership
- Creating symlinks
- Including executables with their runfiles
- Compression (gzip, zstd) and eStargz optimization

Example:

```python
load("@rules_img//img:layer.bzl", "image_layer", "file_metadata")

# Simple layer with files
image_layer(
    name = "app_layer",
    srcs = {
        "/app/bin/server": "//cmd/server",
        "/app/config.json": ":config.json",
    },
)

# Layer with custom permissions
image_layer(
    name = "secure_layer",
    srcs = {
        "/etc/app/config": ":config",
        "/etc/app/secret": ":secret",
    },
    default_metadata = file_metadata(
        mode = "0644",
        uid = 1000,
        gid = 1000,
    ),
    file_metadata = {
        "/etc/app/secret": file_metadata(mode = "0600"),
    },
)

# Layer with symlinks
image_layer(
    name = "bin_layer",
    srcs = {
        "/usr/local/bin/app": "//cmd/app",
    },
    symlinks = {
        "/usr/bin/app": "/usr/local/bin/app",
    },
)
```
""",
    attrs = {
        "srcs": attr.string_keyed_label_dict(
            doc = """Files to include in the layer. Keys are paths in the image (e.g., "/app/bin/server"),
values are labels to files or executables. Executables automatically include their runfiles unless include_runfiles is set to False.""",
            allow_files = True,
        ),
        "symlinks": attr.string_dict(
            doc = """Symlinks to create in the layer. Keys are symlink paths in the image,
values are the targets they point to.""",
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
    } | layer_attrs.common,
    toolchains = TOOLCHAINS,
    provides = [LayersInfo],
)
