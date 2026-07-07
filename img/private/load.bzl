"""Load rule for importing images into a container daemon."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@hermetic_launcher//launcher:lib.bzl", "launcher")
load("//img/private:push_metadata.bzl", "compute_load_metadata")
load("//img/private:root_symlinks.bzl", "calculate_root_symlinks", "symlink_name_prefix")
load("//img/private:stamp.bzl", "expand_or_write")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:default_deploy_tool.bzl", "default_deploy_tool")
load("//img/private/common:deploy_attrs.bzl", "COMMON_LOAD_ATTRS")
load("//img/private/common:deploy_helpers.bzl", "content_tracking_json_vars", "get_image_providers", "get_tags", "image_target_vars", "resolve_daemon", "resolve_load_destination", "resolve_load_strategy")
load("//img/private/common:transitions.bzl", "reset_platform_transition")
load("//img/private/providers:deploy_info.bzl", "DeployInfo")
load("//img/private/providers:deploy_tool_info.bzl", "DeployToolInfo")
load("//img/private/providers:load_settings_info.bzl", "LoadSettingsInfo")
load("//img/private/providers:oci_layout_settings_info.bzl", "OCILayoutSettingsInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")

def _compute_load_metadata(*, ctx, configuration_json):
    manifest_info, index_info = get_image_providers(ctx)
    pull_info = ctx.attr.image[PullInfo] if PullInfo in ctx.attr.image else None

    return compute_load_metadata(
        ctx,
        configuration_json = configuration_json,
        manifest_info = manifest_info,
        index_info = index_info,
        strategy = resolve_load_strategy(ctx),
        pull_info = pull_info,
        output_prefix = ctx.label.name,
    )

def _build_docker_tarball(ctx, configuration_json, manifest_info = None, index_info = None):
    """Build the Docker save tarball for the image.

    The tarball is compatible with both Docker save format (manifest.json)
    and OCI layout (index.json + oci-layout), following the containerd c8d format.

    Args:
        ctx: Rule context.
        configuration_json: The configuration file with expanded templates.
        manifest_info: The ImageManifestInfo provider (for single-platform).
        index_info: The ImageIndexInfo provider (for multi-platform).

    Returns:
        The Docker save tarball file.
    """
    if manifest_info == None and index_info == None:
        fail("_build_docker_tarball requires manifest_info or index_info")
    if manifest_info != None and index_info != None:
        fail("_build_docker_tarball: provide manifest_info or index_info, not both")

    tarball_output = ctx.actions.declare_file(ctx.label.name + "_docker.tar")

    args = ctx.actions.args()
    args.add("docker-save")
    args.add("--output", tarball_output.path)
    args.add("--format", "tar")
    args.add("--configuration-file", configuration_json.path)
    if ctx.attr._oci_layout_settings[OCILayoutSettingsInfo].allow_shallow_oci_layout:
        args.add("--allow-missing-blobs")
    if ctx.attr._oci_ref_name[BuildSettingInfo].value == "tag_only":
        args.add("--oci-ref-name-tag-only")

    inputs = [configuration_json]

    if manifest_info != None:
        args.add("--manifest", manifest_info.manifest.path)
        args.add("--config", manifest_info.config.path)
        inputs.append(manifest_info.manifest)
        inputs.append(manifest_info.config)

        # Add layers with metadata=blob mapping
        for layer in manifest_info.layers:
            if layer.blob != None:
                args.add("--layer", "{}={}".format(layer.metadata.path, layer.blob.path))
                inputs.append(layer.metadata)
                inputs.append(layer.blob)

    if index_info != None:
        args.add("--index", index_info.index.path)
        inputs.append(index_info.index)

        for manifest in index_info.manifests:
            args.add("--manifest-path", manifest.manifest.path)
            args.add("--config-path", manifest.config.path)
            inputs.append(manifest.manifest)
            inputs.append(manifest.config)

            for layer in manifest.layers:
                if layer.blob != None:
                    args.add("--layer", "{}={}".format(layer.metadata.path, layer.blob.path))
                    inputs.append(layer.metadata)
                    inputs.append(layer.blob)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [tarball_output],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        env = {"RULES_IMG": "1"},
        mnemonic = "DockerSave",
    )

    return tarball_output

def _image_load_impl(ctx):
    """Implementation of the load rule."""
    manifest_info, index_info = get_image_providers(ctx)
    image_provider = manifest_info if manifest_info != None else index_info

    strategy = resolve_load_strategy(ctx)
    include_layers = (strategy == "eager")

    root_symlinks_prefix = symlink_name_prefix(ctx)
    root_symlinks = calculate_root_symlinks(index_info, manifest_info, include_layers = include_layers, symlink_name_prefix = root_symlinks_prefix)

    registry, repository = resolve_load_destination(ctx)
    templates = dict(
        registry = registry,
        repository = repository,
        tags = get_tags(ctx),
        daemon = resolve_daemon(ctx),
    )

    # Prepare newline_delimited_lists_files if tag_file is provided
    newline_delimited_lists_files = None
    if ctx.attr.tag_file:
        tag_file = ctx.attr.tag_file.files.to_list()[0]
        newline_delimited_lists_files = {"tags": tag_file}

    # When tracks_content is set, expose the image descriptor as a json-var.
    # The descriptor file becomes an action input (so the tag re-stamps when the
    # digest changes) and is available to templates as {{.digest}}.
    json_vars, json_path_to_root = content_tracking_json_vars(
        image_provider.descriptor if ctx.attr.tracks_content else None,
    )

    # Either expand templates or write directly
    configuration_json = expand_or_write(
        ctx = ctx,
        templates = templates,
        output_name = ctx.label.name + ".configuration.json",
        newline_delimited_lists_files = newline_delimited_lists_files,
        extra_build_settings = image_target_vars(ctx.attr.image.label),
        json_vars = json_vars,
        json_path_to_root = json_path_to_root,
    )

    deploy_metadata, layer_hints = _compute_load_metadata(
        ctx = ctx,
        configuration_json = configuration_json,
    )
    if layer_hints != None:
        root_symlinks["{}layer_hints".format(root_symlinks_prefix)] = layer_hints

    loader = ctx.actions.declare_file(ctx.label.name + ".exe")
    deploy_tool_info = ctx.attr.deploy_tool[DeployToolInfo] if ctx.attr.deploy_tool != None else ctx.attr._deploy_tool[DeployToolInfo]
    embedded_args, transformed_args = launcher.args_from_entrypoint(executable_file = deploy_tool_info.img_deploy_exe)
    embedded_args.extend(["deploy", "--runfiles-root-symlinks-prefix", root_symlinks_prefix, "--request-file"])
    embedded_args, transformed_args = launcher.append_runfile(
        file = deploy_metadata,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
    )
    launcher.compile_stub(
        ctx = ctx,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
        output_file = loader,
        template_file = deploy_tool_info.launcher_template,
    )

    # Build environment for RunEnvironmentInfo
    environment = {
        "IMG_REAPI_ENDPOINT": ctx.attr._load_settings[LoadSettingsInfo].remote_cache,
        "IMG_REAPI_INSTANCE_NAME": ctx.attr._load_settings[LoadSettingsInfo].remote_instance_name,
        "IMG_CREDENTIAL_HELPER": ctx.attr._load_settings[LoadSettingsInfo].credential_helper,
        "IMG_CREDENTIAL_HELPER_OCI_REGISTRY": ctx.attr._load_settings[LoadSettingsInfo].credential_helper_oci_registry,
        "IMG_CREDENTIAL_HELPER_REMOTE_CACHE": ctx.attr._load_settings[LoadSettingsInfo].credential_helper_remote_cache,
    }
    inherited_environment = [
        "IMG_REAPI_ENDPOINT",
        "IMG_REAPI_INSTANCE_NAME",
        "IMG_CREDENTIAL_HELPER",
        "IMG_CREDENTIAL_HELPER_OCI_REGISTRY",
        "IMG_CREDENTIAL_HELPER_REMOTE_CACHE",
        "IMG_AUTH_DEBUG",
        "DOCKER_CONFIG",
        "LOADER_BINARY",
    ]

    # Add REGISTRY_AUTH_FILE if docker_config_path is set
    docker_config_path = ctx.attr._docker_config_path[BuildSettingInfo].value
    if docker_config_path:
        environment["REGISTRY_AUTH_FILE"] = docker_config_path

    output_groups = dict(
        deploy_manifest = depset([deploy_metadata]),
    )

    if manifest_info != None:
        tarball = _build_docker_tarball(ctx, configuration_json, manifest_info = manifest_info)
        output_groups["tarball"] = depset([tarball])
    elif index_info != None:
        tarball = _build_docker_tarball(ctx, configuration_json, index_info = index_info)
        output_groups["tarball"] = depset([tarball])

    return [
        DefaultInfo(
            files = depset([loader]),
            executable = loader,
            runfiles = ctx.runfiles(
                files = [
                    deploy_tool_info.img_deploy_exe,
                    deploy_metadata,
                ],
                root_symlinks = root_symlinks,
            ),
        ),
        OutputGroupInfo(**output_groups),
        RunEnvironmentInfo(
            environment = environment,
            inherited_environment = inherited_environment,
        ),
        DeployInfo(
            image = image_provider,
            deploy_manifest = deploy_metadata,
            layer_hints = layer_hints,
            include_layers = strategy == "eager",
            sign_settings = [],
        ),
    ]

image_load = rule(
    implementation = _image_load_impl,
    doc = """Loads container images into a local daemon (Docker, containerd, or Podman).

This rule creates an executable target that imports OCI images into your local
container runtime. It supports Docker, Podman, and containerd, with intelligent
detection of the best loading method for optimal performance.

Key features:
- **Incremental loading**: Skips blobs that already exist in the daemon
- **Multi-platform support**: Can load entire image indexes or specific platforms
- **Direct containerd integration**: Bypasses Docker for faster imports when possible
- **Platform filtering**: Use `--platform` flag at runtime to select specific platforms

The rule produces an executable that can be run with `bazel run`.

Image names: when loading into a local daemon the image name is ultimately just
a string, so a single fully-qualified `tag` (the rules_oci-compatible form) is
all a daemon needs. The rule also accepts the same `registry` / `repository` /
`tag` split as `image_push`; the loaded name is simply reconstructed as
`{registry}/{repository}:{tag}`. Splitting it out is purely a convenience: it
keeps the load target aligned with a matching `image_push` so the same image is
easy to push to a registry later.

Output groups:
- `tarball`: "docker save" compatible tarball with OCI layout (available for both single and multi-platform images).
  For multi-platform images, the first manifest is used as the default in `manifest.json`,
  and all manifests are included in `index.json`.
  Alternatively, setting `daemon = "tar"` (or `--@rules_img//img/settings:load_daemon=tar`)
  produces the same format on-the-fly by streaming it to stdout at runtime.

Example:

```python
load("@rules_img//img:load.bzl", "image_load")

# Load using separate registry/repository/tag (same API as image_push).
# The loaded image name is reconstructed as gcr.io/my-project/my-app:latest.
image_load(
    name = "load_app",
    image = ":my_app",  # References an image_manifest
    registry = "gcr.io",
    repository = "my-project/my-app",
    tag = "latest",
)

# Load a multi-platform image with the split API
image_load(
    name = "load_multiarch",
    image = ":my_app_index",  # References an image_index
    registry = "gcr.io",
    repository = "my-project/my-app",
    tag = "latest",
    daemon = "containerd",  # Explicitly use containerd
)

# The rules_oci-compatible form still works: a single fully-qualified tag with no
# registry/repository is used verbatim as the loaded image name.
image_load(
    name = "load_legacy",
    image = ":my_app",
    tag = "my-app:latest",
)

# ...including multiple full-reference tags:
image_load(
    name = "load_multi",
    image = ":my_app",
    tag_list = ["my-app:latest", "my-app:v1.0.0", "my-app:stable"],
)

# Load with dynamic tagging (template expansion works with either form)
image_load(
    name = "load_dynamic",
    image = ":my_app",
    tag = "my-app:{{.BUILD_USER}}",  # Template expansion
    build_settings = {
        "BUILD_USER": "//settings:username",
    },
)
```

Runtime usage:
```bash
# Load all platforms
bazel run //path/to:load_app

# Load specific platform only
bazel run //path/to:load_multiarch -- --platform linux/arm64

# Build Docker save tarball
bazel build //path/to:load_app --output_groups=tarball

# Stream tar to stdout (e.g., pipe to another tool)
bazel run //path/to:load_app --@rules_img//img/settings:load_daemon=tar
```

Performance notes:
- When Docker uses containerd storage (Docker 23.0+), images are loaded directly
  into containerd for better performance if the containerd socket is accessible.
- For older Docker versions, falls back to `docker image load` which requires building
  a tar file (slower and limited to single-platform images)
- The `--platform` flag filters which platforms are loaded from multi-platform images
- The `tar` daemon streams a unified OCI+Docker tar to stdout without loading into any daemon
- The `containerization` daemon uses Apple's Containerization framework via `container image load`
""",
    attrs = dict(
        COMMON_LOAD_ATTRS,
        image = attr.label(
            doc = "Image to load. Should provide ImageManifestInfo or ImageIndexInfo.",
            mandatory = True,
        ),
        tool_cfg = attr.string(
            doc = """**Experimental**: This attribute may be removed if we find a way to automatically select the correct loader platform based on the context of use.
Configuration of the loader executable. By default, the loader executable is always chosen for the host platform, regardless of the value of `--platforms`. Setting this attribute to 'target' makes the loader match the target platform instead.
The `"target"` option is useful when the "image_load" target is used as a data dependency of an integration test.

Available options:
- **`host`** (default): Loader executable matches the host platform.
- **`target`**: Loader executable matches the target platform(s) specified via `--platforms`.
""",
            default = "host",
            values = ["host", "target"],
        ),
        deploy_tool = attr.label(
            doc = """Optional label of a deploy tool target providing `DeployToolInfo` (created with `img_deploy_tool` from `@rules_img//img:deploy_tool.bzl`). When set, overrides `tool_cfg`.""",
            mandatory = False,
            providers = [DeployToolInfo],
        ),
        _deploy_tool = attr.label(
            default = default_deploy_tool,
            providers = [DeployToolInfo],
        ),
        _oci_layout_settings = attr.label(
            default = Label("//img/private/settings:oci_layout"),
            providers = [OCILayoutSettingsInfo],
        ),
        _docker_config_path = attr.label(
            default = Label("//img/settings:docker_config_path"),
            providers = [BuildSettingInfo],
        ),
        _oci_ref_name = attr.label(
            default = Label("//img/settings:oci_ref_name"),
            providers = [BuildSettingInfo],
        ),
    ),
    executable = True,
    cfg = reset_platform_transition,
    toolchains = [
        launcher.finalizer_toolchain_type,
    ] + TOOLCHAINS,
)
