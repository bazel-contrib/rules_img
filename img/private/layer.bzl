"""Layer rule for building layers in a container image."""

load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:layer_attrs.bzl", "layer_attrs")
load(
    "//img/private/common:tar_layer.bzl",
    "create_tar_layer",
    "empty_runfile_short_path",
    "file_type",
    "files_arg",
    "get_repo_mapping_manifest",
    "place_extra_executable_files",
    "place_non_executable_files",
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
        is_executable = files_to_run != None and files_to_run.executable != None and not files_to_run.executable.is_source
        if is_executable:
            # This is an executable. Place the executable at path_in_image and any
            # other default outputs relative to it. When include_runfiles is True,
            # also add the runfiles tree.
            executable = files_to_run.executable
            if ctx.attr.include_runfiles:
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
                empty_files_args = ctx.actions.args()
                empty_files_args.set_param_file_format("multiline")
                empty_files_args.use_param_file("--empty-files-from-file=%s", use_always = True)
                empty_files_args.add_all(runfiles.empty_filenames, map_each = empty_runfile_short_path, format_each = "{}.runfiles/%s".format(path_in_image))
                extra_args.append(empty_files_args)
                repo_mapping_manifest = get_repo_mapping_manifest(files)
                if repo_mapping_manifest != None:
                    extra_inputs.append(depset([repo_mapping_manifest]))
                    files_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}.repo_mapping\0%s".format(path_in_image), expand_directories = False)
                    files_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}.runfiles/_repo_mapping\0%s".format(path_in_image), expand_directories = False)
            else:
                # Only the executable itself, without runfiles.
                files_args.add_all(["{}\0{}{}".format(path_in_image, file_type(executable), executable.path)])

            # Copy any additional default outputs (beyond the executable), placed
            # relative to the executable's location.
            place_extra_executable_files(ctx, default_info.files, executable, path_in_image, extra_args, extra_inputs)
            continue

        # This isn't an executable. Add all default outputs, placing them according
        # to multi_file_layout when the target produces more than one output.
        place_non_executable_files(ctx, default_info.files, files.label, path_in_image, ctx.attr.multi_file_layout, extra_args, extra_inputs)

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
- Including executables with their runfiles and any additional default outputs
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

### Output groups

- `mtree`: a single [mtree](https://man.freebsd.org/cgi/man.cgi?mtree(5)) text file
""",
    attrs = {
        "srcs": attr.string_keyed_label_dict(
            doc = """Files to include in the layer. Keys are paths in the image (e.g., "/app/bin/server"),
values are labels to files or executables.

When a value is an executable, the executable is placed at the path key and its runfiles tree is
included (unless include_runfiles is set to False). Any additional default outputs of the target
(the rest of `DefaultInfo.files` beyond the executable) are also copied, each placed at the same
location relative to the executable that it has in the source tree.

When a value is a non-executable target that produces more than one default output, the path key is
treated as a directory and the outputs are placed inside it according to `multi_file_layout`.""",
            allow_files = True,
        ),
        "symlinks": attr.string_dict(
            doc = """Symlinks to create in the layer. Keys are symlink paths in the image,
values are the targets they point to.""",
        ),
        "multi_file_layout": attr.string(
            default = "package_relative",
            values = ["package_relative", "flatten"],
            doc = """How to place a non-executable src that produces MORE THAN ONE default output.

- `"package_relative"` (default): treat the path key as a directory and place each file inside it,
  preserving its path relative to the producing target's package.
- `"flatten"`: place each file directly in the directory by basename (restores the older behavior).

A src that produces a single output is always placed exactly at its path key, regardless of this
setting.""",
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
