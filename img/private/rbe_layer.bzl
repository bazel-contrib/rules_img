"""RBE Layer rule for building Remote Execution API Directory trees."""

load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")

def _file_type(f):
    type = "f"  # regular file
    if f.is_directory:
        type = "d"
    if hasattr(f, "is_symlink") and f.is_symlink:
        type = "l"
    return type

def _files_arg(f):
    type = _file_type(f)
    return "{}{}".format(type, f.path)

def _to_short_path_pair(f):
    type = _file_type(f)
    if f.short_path.startswith("../"):
        return "{}\0{}{}".format(f.short_path[3:], type, f.path)
    return "_main/{}\0{}{}".format(f.short_path, type, f.path)

def _root_symlinks_arg(x):
    type = _file_type(x.target_file)
    return "{}\0{}{}".format(x.path, type, x.target_file.path)

def _symlinks_arg(x):
    type = _file_type(x.target_file)
    return "{}\0{}{}_main/{}".format(x.path, type, x.target_file.path)

def _get_files_to_run_provider(src):
    """Retrieve FilesToRunProvider from a target.

    Args:
        src: target to get FilesToRunProvider from

    Returns:
        FilesToRunProvider or None: FilesToRunProvider if found in target
            provider, otherwise None
    """
    if not DefaultInfo in src:
        return None
    default_info = src[DefaultInfo]
    if not hasattr(default_info, "files_to_run"):
        return None
    return default_info.files_to_run

def _get_repo_mapping_manifest(src):
    """Retrieve repo_mapping_manifest from a target if it exists.

    Args:
        src: target to get repo_mapping_manifest from

    Returns:
        File or None: repo_mapping_manifest
    """
    files_to_run_provider = _get_files_to_run_provider(src)
    if files_to_run_provider:
        # repo_mapping_manifest is Bazel 7+ only
        return getattr(files_to_run_provider, "repo_mapping_manifest")
    return None

def _image_rbe_layer_impl(ctx):
    # Declare outputs
    digest_out = ctx.actions.declare_file(ctx.attr.name + ".root")
    protos_out = ctx.actions.declare_directory(ctx.attr.name + ".protos")

    # Create temp file for inputs list
    inputs_file = ctx.actions.declare_file(ctx.attr.name + "_inputs.txt")

    # Create input file list (same format as image_layer)
    files_args = ctx.actions.args()
    files_args.set_param_file_format("multiline")

    inputs = []

    # Process srcs (same logic as image_layer)
    for (path_in_image, files) in ctx.attr.srcs.items():
        path_in_image = path_in_image.removeprefix("/")  # the "/" is not included
        default_info = files[DefaultInfo]
        files_to_run = default_info.files_to_run
        executable = None
        runfiles = None
        inputs.append(default_info.files)

        if ctx.attr.include_runfiles and files_to_run != None and files_to_run.executable != None and not files_to_run.executable.is_source:
            # This is an executable with runfiles
            executable = files_to_run.executable
            runfiles = default_info.default_runfiles

            # Add the executable itself
            files_args.add_all([executable], map_each = _files_arg, format_each = "{}\0%s".format(path_in_image), expand_directories = False)

            # Add runfiles
            files_args.add_all(runfiles.files, map_each = _to_short_path_pair, format_each = "{}.runfiles/%s".format(path_in_image), expand_directories = False, uniquify = True)
            files_args.add_all(runfiles.symlinks, map_each = _symlinks_arg, format_each = "{}.runfiles/%s".format(path_in_image))
            files_args.add_all(runfiles.root_symlinks, map_each = _root_symlinks_arg, format_each = "{}.runfiles/%s".format(path_in_image))

            inputs.append(runfiles.files)
            inputs.append(runfiles.symlinks)
            inputs.append(runfiles.root_symlinks)

            # Handle repo_mapping_manifest
            repo_mapping_manifest = _get_repo_mapping_manifest(files)
            if repo_mapping_manifest != None:
                inputs.append(depset([repo_mapping_manifest]))
                files_args.add_all([repo_mapping_manifest], map_each = _files_arg, format_each = "{}.repo_mapping\0%s".format(path_in_image), expand_directories = False)
                files_args.add_all([repo_mapping_manifest], map_each = _files_arg, format_each = "{}.runfiles/_repo_mapping\0%s".format(path_in_image), expand_directories = False)
            continue

        # This isn't an executable (or include_runfiles is False)
        # Add all files instead
        if default_info.files == None:
            fail("Expected {} ({}) to contain an executable or files, got None".format(path_in_image, files))
        files_args.add_all(default_info.files, map_each = _files_arg, format_each = "{}\0%s".format(path_in_image), expand_directories = False)

    # Add explicit symlinks
    if len(ctx.attr.symlinks) > 0:
        for (source, dest) in ctx.attr.symlinks.items():
            source = source.removeprefix("/")
            files_args.add("{}\0l{}".format(source, dest))

    # Write the inputs list to a file
    ctx.actions.write(
        output = inputs_file,
        content = files_args,
    )

    # Build main argument list with param file support
    args = ctx.actions.args()
    args.add("--digest-function", ctx.attr.digest_function)
    args.add("--inputs", inputs_file)
    args.add("--digest-output", digest_out.path)
    args.add("--proto-output-dir", protos_out.path)
    args.set_param_file_format("multiline")
    args.use_param_file("@%s", use_always = True)

    # Run the dirtree command with persistent worker
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        outputs = [digest_out, protos_out],
        inputs = depset([inputs_file], transitive = inputs),
        executable = img_toolchain_info.tool_exe,
        arguments = ["dirtree", args],
        mnemonic = "RBELayerTree",
        execution_requirements = {
            "requires-worker-protocol": "json",
            "supports-workers": "1",
            "supports-multiplex-workers": "1",
            "supports-multiplex-sandboxing": "1",
            "supports-worker-cancellation": "1",
            "supports-path-mapping": "1",
        },
    )

    return [
        DefaultInfo(files = depset([digest_out])),
        OutputGroupInfo(
            protos = depset([protos_out]),
            digest = depset([digest_out]),
        ),
    ]

image_rbe_layer = rule(
    implementation = _image_rbe_layer_impl,
    doc = """Creates a Remote Execution API Directory tree from files, executables, and directories.

This rule builds a directory tree representation compatible with the Remote Execution API
instead of creating a tar file. It outputs:
- A digest file containing the root directory hash
- A directory of proto messages (Directory protos) for the entire tree

This is useful for integration with Remote Build Execution systems and content-addressable storage.

Example:

```python
load("@rules_img//img:rbe_layer.bzl", "image_rbe_layer")

# Simple RBE layer with files
image_rbe_layer(
    name = "app_rbe_layer",
    srcs = {
        "/app/bin/server": "//cmd/server",
        "/app/config.json": ":config.json",
    },
)

# RBE layer with symlinks
image_rbe_layer(
    name = "bin_rbe_layer",
    srcs = {
        "/usr/local/bin/app": "//cmd/app",
    },
    symlinks = {
        "/usr/bin/app": "/usr/local/bin/app",
    },
)

# Use different hash function
image_rbe_layer(
    name = "app_blake3_layer",
    srcs = {
        "/app/bin/server": "//cmd/server",
    },
    digest_function = "blake3",
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
        "include_runfiles": attr.bool(
            default = True,
            doc = """Whether to include runfiles for executable targets.
When True (default), executables in srcs will include their runfiles tree.
When False, only the executable file itself is included, without runfiles.""",
        ),
        "digest_function": attr.string(
            default = "sha256",
            doc = """Hash function to use for computing digests.
Supported values: sha1, sha256 (default), sha384, sha512, blake3.
Also accepts Bazel-style names like SHA-256, SHA256, etc.""",
        ),
    },
    toolchains = TOOLCHAINS,
)
