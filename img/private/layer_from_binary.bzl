"""Binary layer rule for packaging a *_binary target into a container image layer."""

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
load("//img/private/providers:layer_config_info.bzl", "ImageLayerConfigInfo")
load("//img/private/providers:layer_info.bzl", "LayerInfo")

_BinaryRunInfo = provider(
    doc = """\
This provider is only used by a private aspect and shouldn't be visible outside the layer_from_binary rule.
Collects args and env of a *_binary target.
""",
    fields = dict(
        args = "Arguments of the *_binary target",
        env = "Environment variables of the *_binary target",
    ),
)

def _binary_run_info_extraction_aspect_impl(target, ctx):
    # https://bazel.build/reference/be/common-definitions#common-attributes-binaries
    # Find "args" attribute (list of strings)
    # Find RunEnvironmentInfo or "env" attribute (string -> string dict)
    extracted_args = []
    extracted_env = {}

    targets_for_expansion = [target]
    if hasattr(ctx.rule.attr, "data") and type(ctx.rule.attr.data) == type([]):
        # Collect data for expansion
        targets_for_expansion.extend(ctx.rule.attr.data)
    if hasattr(ctx.rule.attr, "args"):
        if type(ctx.rule.attr.args) != type([]):
            fail("Expected args to be a list, got", type(ctx.rule.attr.args))
        for arg in ctx.rule.attr.args:
            arg = ctx.expand_location(arg, targets = targets_for_expansion)
            arg = ctx.expand_make_variables("args", arg, {})
            extracted_args.append(arg)
    if RunEnvironmentInfo in target:
        env_info = target[RunEnvironmentInfo]
        extracted_env.update(env_info.environment)
    elif hasattr(ctx.rule.attr, "env"):
        env_attr = ctx.rule.attr.env
        if type(ctx.rule.attr.env) != type({}):
            fail("Expected env to be a dict, got", type(env_attr))
        for k, v in env_attr.items():
            v = ctx.expand_location(v, targets = targets_for_expansion)
            v = ctx.expand_make_variables("env", v, {})
            extracted_env[k] = v
    return [_BinaryRunInfo(
        args = extracted_args,
        env = extracted_env,
    )]

_binary_run_info_extraction_aspect = aspect(
    implementation = _binary_run_info_extraction_aspect_impl,
    attr_aspects = [],  # The aspect only inspect the target itself (not the deps)
    provides = [_BinaryRunInfo],
)

def _layer_from_binary_impl(ctx):
    run_info = ctx.attr.binary[_BinaryRunInfo]
    exe = ctx.executable.binary
    path_in_image = ctx.attr.path
    if len(path_in_image) == 0:
        if exe.short_path.startswith("../"):
            path_in_image = exe.short_path[3:]
        else:
            path_in_image = "_main/{}".format(exe.short_path)
    elif path_in_image.endswith("/"):
        path_in_image = "{prefix}{basename}".format(
            prefix = path_in_image,
            basename = exe.basename,
        )
    absolute_entrypoint = path_in_image if path_in_image.startswith("/") else "/" + path_in_image
    working_dir = None
    if ctx.attr.include_runfiles:
        working_dir = "{}.runfiles/_main".format(absolute_entrypoint)

    settings = resolve_layer_settings(ctx)

    extra_args = []
    extra_inputs = []

    default_info = ctx.attr.binary[DefaultInfo]
    extra_inputs.append(default_info.files)

    if ctx.attr.include_runfiles:
        extra_args.append("--executable={}={}".format(path_in_image, exe.path))

        runfiles = default_info.default_runfiles
        if runfiles:
            runfiles_args = ctx.actions.args()
            runfiles_args.set_param_file_format("multiline")
            runfiles_args.use_param_file("--runfiles={}=%s".format(exe.path), use_always = True)
            runfiles_args.add_all(runfiles.files, map_each = to_short_path_pair, expand_directories = False, uniquify = True)
            runfiles_args.add_all(runfiles.symlinks, map_each = symlinks_arg)
            runfiles_args.add_all(runfiles.root_symlinks, map_each = root_symlinks_arg)
            extra_args.append(runfiles_args)
            extra_inputs.append(runfiles.files)

            symlink_inputs = []
            symlink_inputs.extend([symlink_entry.target_file for symlink_entry in runfiles.symlinks.to_list()])
            symlink_inputs.extend([symlink_entry.target_file for symlink_entry in runfiles.root_symlinks.to_list()])
            if len(symlink_inputs) > 0:
                extra_inputs.append(depset(symlink_inputs))

        repo_mapping_manifest = get_repo_mapping_manifest(ctx.attr.binary)
        if repo_mapping_manifest != None:
            extra_inputs.append(depset([repo_mapping_manifest]))
            repo_mapping_args = ctx.actions.args()
            repo_mapping_args.set_param_file_format("multiline")
            repo_mapping_args.use_param_file("--add-from-file=%s", use_always = True)
            repo_mapping_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}.repo_mapping\0%s".format(path_in_image), expand_directories = False)
            repo_mapping_args.add_all([repo_mapping_manifest], map_each = files_arg, format_each = "{}.runfiles/_repo_mapping\0%s".format(path_in_image), expand_directories = False)
            extra_args.append(repo_mapping_args)
    else:
        binary_file_args = ctx.actions.args()
        binary_file_args.set_param_file_format("multiline")
        binary_file_args.use_param_file("--add-from-file=%s", use_always = True)
        binary_file_args.add_all(default_info.files, map_each = files_arg, format_each = "{}\0%s".format(path_in_image), expand_directories = False)
        extra_args.append(binary_file_args)

    result = create_tar_layer(ctx, settings, extra_args = extra_args, extra_inputs = extra_inputs)
    return result + [
        ImageLayerConfigInfo(
            entrypoint = [absolute_entrypoint],
            cmd = run_info.args,
            env = run_info.env,
            working_dir = working_dir,
        ),
    ]

layer_from_binary = rule(
    implementation = _layer_from_binary_impl,
    doc = """Creates a container image layer from a *_binary target.

This rule packages a binary executable and its runfiles into a layer, and additionally
provides image configuration (entrypoint, cmd, env, working_dir) via ImageLayerConfigInfo.
When used as a layer in image_manifest, the configuration is automatically applied to the
image with Dockerfile-like semantics.

The binary's `args` attribute becomes the image `cmd`, its `env` attribute (or
RunEnvironmentInfo provider) becomes `env`, and the binary path becomes the `entrypoint`.
When include_runfiles is True (default), the working directory is set to the runfiles root.

Example:

```python
load("@rules_img//img:layer.bzl", "layer_from_binary")
load("@rules_img//img:image.bzl", "image_manifest")

# Package a Go binary with its runfiles
layer_from_binary(
    name = "app_layer",
    binary = "//cmd/server",
)

# Use in an image - entrypoint, cmd, env, and working_dir are set automatically
image_manifest(
    name = "image",
    base = "@distroless_base",
    layers = [":app_layer"],
)

# Override the path inside the image
layer_from_binary(
    name = "custom_path_layer",
    binary = "//cmd/server",
    path = "/usr/local/bin/",
)

# Without runfiles (static binary)
layer_from_binary(
    name = "static_layer",
    binary = "//cmd/server",
    path = "/usr/local/bin/server",
    include_runfiles = False,
)
```
""",
    attrs = {
        "binary": attr.label(
            doc = """The *_binary target to package into the layer.

The binary's `args` and `env` attributes are extracted and provided as image configuration
(cmd and env) via ImageLayerConfigInfo. The `data` attribute is used for `$(location)` expansion
in args and env values.""",
            executable = True,
            mandatory = True,
            cfg = "target",
            aspects = [_binary_run_info_extraction_aspect],
        ),
        "path": attr.string(
            mandatory = False,
            doc = """\
Optional path of the binary inside the image.
If the path ends with a slash ("/"), the basename of the binary will be automatically appended.
If unset, this defaults to the rlocationpath of the binary (e.g., "_main/cmd/server/server_/server").
""",
        ),
    } | layer_attrs.common,
    toolchains = TOOLCHAINS,
    provides = [LayerInfo],
)
