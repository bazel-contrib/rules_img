"""Load rule for importing images into a container daemon."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@hermetic_launcher//launcher:lib.bzl", "launcher")
load("@platforms//host:constraints.bzl", "HOST_CONSTRAINTS")
load("//img/private:root_symlinks.bzl", "calculate_root_symlinks", "symlink_name_prefix")
load("//img/private:stamp.bzl", "expand_or_write")
load("//img/private/common:build.bzl", "DATA_TOOLCHAIN", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:transitions.bzl", "reset_platform_transition")
load("//img/private/providers:deploy_info.bzl", "DeployInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:load_settings_info.bzl", "LoadSettingsInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:oci_layout_settings_info.bzl", "OCILayoutSettingsInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

def _load_strategy(ctx):
    """Determine the load strategy to use based on the settings."""
    load_settings = ctx.attr._load_settings[LoadSettingsInfo]
    strategy = ctx.attr.strategy
    if strategy == "auto":
        strategy = load_settings.strategy
    return strategy

def _daemon(ctx):
    """Determine the daemon to target based on the settings."""
    load_settings = ctx.attr._load_settings[LoadSettingsInfo]
    daemon = ctx.attr.daemon
    if daemon == "auto":
        daemon = load_settings.daemon
    return daemon

def _get_tags(ctx):
    """Get the list of tags from the context, validating mutual exclusivity."""
    if ctx.attr.tag and ctx.attr.tag_list:
        fail("Cannot specify both 'tag' and 'tag_list' attributes")

    tags = []
    if ctx.attr.tag:
        tags = [ctx.attr.tag]
    elif ctx.attr.tag_list:
        tags = ctx.attr.tag_list

    # tag_file is handled separately via newline_delimited_lists_files and will be merged

    return tags

def _target_info(ctx):
    pull_info = ctx.attr.image[PullInfo] if PullInfo in ctx.attr.image else None
    if pull_info == None:
        return {}
    return dict(
        original_registries = pull_info.registries,
        original_repository = pull_info.repository,
        original_tag = pull_info.tag,
        original_digest = pull_info.digest,
    )

def _compute_load_metadata(*, ctx, configuration_json):
    inputs = [configuration_json]
    args = ctx.actions.args()
    args.add("deploy-metadata")
    args.add("--command", "load")
    manifest_info = ctx.attr.image[ImageManifestInfo] if ImageManifestInfo in ctx.attr.image else None
    index_info = ctx.attr.image[ImageIndexInfo] if ImageIndexInfo in ctx.attr.image else None
    if manifest_info == None and index_info == None:
        fail("image must provide ImageManifestInfo or ImageIndexInfo")
    if manifest_info != None and index_info != None:
        fail("image must provide either ImageManifestInfo or ImageIndexInfo, not both")
    args.add("--strategy", _load_strategy(ctx))
    args.add("--configuration-file", configuration_json.path)
    target_info = _target_info(ctx)
    if "original_registries" in target_info:
        args.add_all(target_info["original_registries"], before_each = "--original-registry")
    if "original_repository" in target_info:
        args.add("--original-repository", target_info["original_repository"])
    if "original_tag" in target_info and target_info["original_tag"] != None:
        args.add("--original-tag", target_info["original_tag"])
    if "original_digest" in target_info and target_info["original_digest"] != None:
        args.add("--original-digest", target_info["original_digest"])

    if manifest_info != None:
        args.add("--root-path", manifest_info.manifest.path)
        args.add("--root-kind", "manifest")
        args.add("--manifest-path", "0=" + manifest_info.manifest.path)
        args.add("--missing-blobs-for-manifest", "0=" + (",".join(manifest_info.missing_blobs)))
        inputs.append(manifest_info.manifest)
    if index_info != None:
        args.add("--root-path", index_info.index.path)
        args.add("--root-kind", "index")
        for i, manifest in enumerate(index_info.manifests):
            args.add("--manifest-path", "{}={}".format(i, manifest.manifest.path))
            args.add("--missing-blobs-for-manifest", "{}={}".format(i, ",".join(manifest.missing_blobs)))
        inputs.append(index_info.index)
        inputs.extend([manifest.manifest for manifest in index_info.manifests])

    metadata_out = ctx.actions.declare_file(ctx.label.name + ".json")
    args.add(metadata_out.path)
    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [metadata_out],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "LoadMetadata",
    )
    return metadata_out

def _build_docker_tarball(ctx, configuration_json, manifest_info):
    """Build the Docker save tarball for the image.

    Args:
        ctx: Rule context.
        configuration_json: The configuration file with expanded templates.
        manifest_info: The ImageManifestInfo provider.

    Returns:
        The Docker save tarball file.
    """
    tarball_output = ctx.actions.declare_file(ctx.label.name + "_docker.tar")

    args = ctx.actions.args()
    args.add("docker-save")
    args.add("--manifest", manifest_info.manifest.path)
    args.add("--config", manifest_info.config.path)
    args.add("--output", tarball_output.path)
    args.add("--format", "tar")
    args.add("--configuration-file", configuration_json.path)
    if ctx.attr._oci_layout_settings[OCILayoutSettingsInfo].allow_shallow_oci_layout:
        args.add("--allow-missing-blobs")

    inputs = [manifest_info.manifest, manifest_info.config, configuration_json]

    # Add layers with metadata=blob mapping
    for layer in manifest_info.layers:
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
    manifest_info = ctx.attr.image[ImageManifestInfo] if ImageManifestInfo in ctx.attr.image else None
    index_info = ctx.attr.image[ImageIndexInfo] if ImageIndexInfo in ctx.attr.image else None
    if manifest_info == None and index_info == None:
        fail("image must provide ImageManifestInfo or ImageIndexInfo")
    if manifest_info != None and index_info != None:
        fail("image must provide either ImageManifestInfo or ImageIndexInfo, not both")
    image_provider = manifest_info if manifest_info != None else index_info

    strategy = _load_strategy(ctx)
    include_layers = (strategy == "eager")

    root_symlinks_prefix = symlink_name_prefix(ctx)
    root_symlinks = calculate_root_symlinks(index_info, manifest_info, include_layers = include_layers, symlink_name_prefix = root_symlinks_prefix)

    templates = dict(
        tags = _get_tags(ctx),
        daemon = _daemon(ctx),
    )

    # Prepare newline_delimited_lists_files if tag_file is provided
    newline_delimited_lists_files = None
    if ctx.attr.tag_file:
        tag_file = ctx.attr.tag_file.files.to_list()[0]
        newline_delimited_lists_files = {"tags": tag_file}

    # Either expand templates or write directly
    configuration_json = expand_or_write(
        ctx = ctx,
        templates = templates,
        output_name = ctx.label.name + ".configuration.json",
        newline_delimited_lists_files = newline_delimited_lists_files,
    )

    deploy_metadata = _compute_load_metadata(
        ctx = ctx,
        configuration_json = configuration_json,
    )
    loader = ctx.actions.declare_file(ctx.label.name + ".exe")
    if ctx.attr.tool_cfg == "host":
        img_toolchain_info = ctx.exec_groups["host"].toolchains[TOOLCHAIN].imgtoolchaininfo
        template_exec_group = "host"
        template_cfg = "exec"
    elif ctx.attr.tool_cfg == "target":
        img_toolchain_info = ctx.toolchains[DATA_TOOLCHAIN].imgtoolchaininfo
        template_exec_group = None
        template_cfg = "target"
    else:
        fail("Invalid tool_cfg: {}".format(ctx.attr.tool_cfg))

    embedded_args, transformed_args = launcher.args_from_entrypoint(executable_file = img_toolchain_info.tool_exe)
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
        cfg = template_cfg,
        template_exec_group = template_exec_group,
    )

    # Build environment for RunEnvironmentInfo
    environment = {
        "IMG_REAPI_ENDPOINT": ctx.attr._load_settings[LoadSettingsInfo].remote_cache,
        "IMG_CREDENTIAL_HELPER": ctx.attr._load_settings[LoadSettingsInfo].credential_helper,
    }
    inherited_environment = [
        "IMG_REAPI_ENDPOINT",
        "IMG_CREDENTIAL_HELPER",
        "DOCKER_CONFIG",
        "LOADER_BINARY",
    ]

    # Add REGISTRY_AUTH_FILE if docker_config_path is set
    docker_config_path = ctx.attr._docker_config_path[BuildSettingInfo].value
    if docker_config_path:
        environment["REGISTRY_AUTH_FILE"] = docker_config_path

    providers = [
        DefaultInfo(
            files = depset([loader]),
            executable = loader,
            runfiles = ctx.runfiles(
                files = [
                    img_toolchain_info.tool_exe,
                    deploy_metadata,
                ],
                root_symlinks = root_symlinks,
            ),
        ),
        RunEnvironmentInfo(
            environment = environment,
            inherited_environment = inherited_environment,
        ),
        DeployInfo(
            image = image_provider,
            deploy_manifest = deploy_metadata,
        ),
    ]

    # Add tarball output group only for single-platform images (manifest_info)
    # Index info (multi-platform) is not supported by docker-save command
    if manifest_info != None:
        tarball = _build_docker_tarball(ctx, configuration_json, manifest_info)
        providers.append(OutputGroupInfo(
            tarball = depset([tarball]),
        ))

    return providers

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

Output groups:
- `tarball`: Docker save compatible tarball (only available for single-platform images)

Example:

```python
load("@rules_img//img:load.bzl", "image_load")

# Load a single-platform image with a single tag
image_load(
    name = "load_app",
    image = ":my_app",  # References an image_manifest
    tag = "my-app:latest",
)

# Load with multiple tags
image_load(
    name = "load_multi",
    image = ":my_app",
    tag_list = ["my-app:latest", "my-app:v1.0.0", "my-app:stable"],
)

# Load a multi-platform image
image_load(
    name = "load_multiarch",
    image = ":my_app_index",  # References an image_index
    tag = "my-app:latest",
    daemon = "containerd",  # Explicitly use containerd
)

# Load with dynamic tagging
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
```

Performance notes:
- When Docker uses containerd storage (Docker 23.0+), images are loaded directly
  into containerd for better performance if the containerd socket is accessible.
- For older Docker versions, falls back to `docker load` which requires building
  a tar file (slower and limited to single-platform images)
- The `--platform` flag filters which platforms are loaded from multi-platform images
""",
    attrs = {
        "image": attr.label(
            doc = "Image to load. Should provide ImageManifestInfo or ImageIndexInfo.",
            mandatory = True,
        ),
        "daemon": attr.string(
            doc = """Container daemon to use for loading the image.

Available options:
- **`auto`** (default): Uses the global default setting (usually `docker`)
- **`containerd`**: Loads directly into containerd namespace. Supports multi-platform images
  and incremental loading.
- **`docker`**: Loads via Docker daemon. When Docker uses containerd storage (23.0+),
  loads directly into containerd. Otherwise falls back to `docker load` command which
  is slower and limited to single-platform images.
- **`podman`**: Loads via Podman daemon using `podman load` command. Similar to Docker
  fallback mode, this is slower than containerd and limited to single-platform images.
- **`generic`**: Loads via a custom container runtime. The loader will invoke the command
  specified in the `LOADER_BINARY` environment variable with the `load` subcommand. For example,
  if `LOADER_BINARY=nerdctl`, it will run `nerdctl load`. Limited to single-platform images.
  Requires `LOADER_BINARY` to be set at runtime.

The best performance is achieved with:
- Direct containerd access (daemon = "containerd")
- Docker 23.0+ with containerd storage enabled and accessible containerd socket
""",
            default = "auto",
            values = ["auto", "docker", "containerd", "podman", "generic"],
        ),
        "tag": attr.string(
            doc = """Tag to apply when loading the image.

Optional - if omitted, the image is loaded without a tag.

Subject to [template expansion](/docs/templating.md).
""",
        ),
        "tag_list": attr.string_list(
            doc = """List of tags to apply when loading the image.

Useful for applying multiple tags in a single load:

```python
tag_list = ["latest", "v1.0.0", "stable"]
```

Cannot be used together with `tag`. Can be combined with `tag_file` to merge tags from both sources.
Each tag is subject to [template expansion](/docs/templating.md).
""",
        ),
        "tag_file": attr.label(
            doc = """File containing newline-delimited tags to apply when loading the image.

The file should contain one tag per line. Empty lines are ignored. Tags from this file
are merged with tags specified via `tag` or `tag_list` attributes.

Example file content:
```
latest
v1.0.0
stable
```

Can be combined with `tag` or `tag_list` to merge tags from multiple sources.
Each tag is subject to [template expansion](/docs/templating.md).
""",
            allow_single_file = True,
        ),
        "strategy": attr.string(
            doc = """Strategy for handling image layers during load.

Available strategies:
- **`auto`** (default): Uses the global default load strategy
- **`eager`**: Downloads all layers during the build phase. Ensures all layers are
  available locally before running the load command.
- **`lazy`**: Downloads layers only when needed during the load operation. More
  efficient for large images where some layers might already exist in the daemon.
""",
            default = "auto",
            values = ["auto", "eager", "lazy"],
        ),
        "build_settings": attr.string_keyed_label_dict(
            doc = "Build settings to use for [template expansion](/docs/templating.md). Keys are setting names, values are labels to string_flag targets.",
            providers = [BuildSettingInfo],
        ),
        "stamp": attr.string(
            doc = "Whether to use stamping for [template expansion](/docs/templating.md). If 'enabled', uses volatile-status.txt and version.txt if present. 'auto' uses the global default setting.",
            default = "auto",
            values = ["auto", "enabled", "disabled"],
        ),
        "tool_cfg": attr.string(
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
        "_load_settings": attr.label(
            default = Label("//img/private/settings:load"),
            providers = [LoadSettingsInfo],
        ),
        "_stamp_settings": attr.label(
            default = Label("//img/private/settings:stamp"),
            providers = [StampSettingInfo],
        ),
        "_oci_layout_settings": attr.label(
            default = Label("//img/private/settings:oci_layout"),
            providers = [OCILayoutSettingsInfo],
        ),
        "_docker_config_path": attr.label(
            default = Label("//img/settings:docker_config_path"),
            providers = [BuildSettingInfo],
        ),
    },
    executable = True,
    cfg = reset_platform_transition,
    exec_groups = {
        "host": exec_group(
            exec_compatible_with = HOST_CONSTRAINTS,
            toolchains = [launcher.template_exec_toolchain_type] + TOOLCHAINS,
        ),
    },
    toolchains = [
        launcher.finalizer_toolchain_type,
        launcher.template_toolchain_type,
        DATA_TOOLCHAIN,
    ] + TOOLCHAINS,
)
