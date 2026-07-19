"""Push rule for uploading images to a registry."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@hermetic_launcher//launcher:lib.bzl", "launcher")
load("//img/private:push_metadata.bzl", "compute_push_metadata")
load("//img/private:root_symlinks.bzl", "calculate_root_symlinks", "symlink_name_prefix")
load("//img/private:sign_settings.bzl", "add_sign_setting_symlinks")
load("//img/private:stamp.bzl", "expand_or_write")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/common:default_deploy_tool.bzl", "default_deploy_tool")
load("//img/private/common:deploy_attrs.bzl", "COMMON_PUSH_ATTRS")
load("//img/private/common:deploy_helpers.bzl", "content_tracking_json_vars", "extract_cross_mount_from", "extract_referrers", "get_image_providers", "get_tags", "image_target_vars", "resolve_push_registry", "resolve_push_strategy", "resolve_signing")
load("//img/private/common:transitions.bzl", "reset_platform_transition")
load("//img/private/providers:deploy_info.bzl", "DeployInfo")
load("//img/private/providers:deploy_tool_info.bzl", "DeployToolInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")
load("//img/private/providers:push_settings_info.bzl", "PushSettingsInfo")

def _per_child_manifest_tag_file(*, ctx, child_index, child_info):
    extra = image_target_vars(ctx.attr.image.label)
    extra.update({
        "os": child_info.os or "",
        "architecture": child_info.architecture or "",
        "arch": child_info.architecture or "",
        "cpu": child_info.architecture or "",
        "variant": child_info.variant or "",
    })
    templates = dict(manifest_tags = ctx.attr.manifest_tags)
    return expand_or_write(
        ctx = ctx,
        templates = templates,
        output_name = "{}.manifest_tags.{}.json".format(ctx.label.name, child_index),
        extra_build_settings = extra,
    )

def _compute_push_metadata(*, ctx, configuration_json, destination_file = None, signing = None):
    manifest_info, index_info = get_image_providers(ctx)
    pull_info = ctx.attr.image[PullInfo] if PullInfo in ctx.attr.image else None

    manifest_tags_expanded = []
    if index_info != None and ctx.attr.manifest_tags:
        for i, manifest in enumerate(index_info.manifests):
            tag_file = _per_child_manifest_tag_file(ctx = ctx, child_index = i, child_info = manifest)
            if tag_file != None:
                manifest_tags_expanded.append((i, tag_file))

    return compute_push_metadata(
        ctx,
        configuration_json = configuration_json,
        manifest_info = manifest_info,
        index_info = index_info,
        strategy = resolve_push_strategy(ctx),
        cross_mount_strategy = ctx.attr._push_settings[PushSettingsInfo].cross_mount,
        cross_mount_from = extract_cross_mount_from(ctx),
        referrers = extract_referrers(ctx),
        manifest_tags_expanded = manifest_tags_expanded,
        pull_info = pull_info,
        destination_file = destination_file,
        output_prefix = ctx.label.name,
        signing = signing,
    )

def _image_push_impl(ctx):
    """Implementation of the push rule."""
    manifest_info, index_info = get_image_providers(ctx)
    image_provider = manifest_info if manifest_info != None else index_info

    if ctx.attr.manifest_tags and index_info == None:
        fail("'manifest_tags' can only be used when 'image' is an image_index")

    signing = resolve_signing(ctx)

    registry = resolve_push_registry(ctx)

    templates = dict(
        registry = registry,
        repository = ctx.attr.repository,
        tags = get_tags(ctx),
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

    deploy_metadata, layer_hints = _compute_push_metadata(
        ctx = ctx,
        configuration_json = configuration_json,
        destination_file = ctx.file.destination_file,
        signing = signing,
    )
    push_strategy = resolve_push_strategy(ctx)
    root_symlinks_prefix = symlink_name_prefix(ctx)
    root_symlinks = calculate_root_symlinks(
        index_info,
        manifest_info,
        include_layers = push_strategy == "eager",
        symlink_name_prefix = root_symlinks_prefix,
    )

    # Add referrer root symlinks (operation_index starts at 1; main image is 0)
    for ref_idx, referrer in enumerate(ctx.attr.referrers):
        ref_manifest_info = referrer[ImageManifestInfo] if ImageManifestInfo in referrer else None
        ref_index_info = referrer[ImageIndexInfo] if ImageIndexInfo in referrer else None
        root_symlinks.update(calculate_root_symlinks(
            ref_index_info,
            ref_manifest_info,
            include_layers = push_strategy == "eager",
            symlink_name_prefix = root_symlinks_prefix,
            operation_index = ref_idx + 1,
        ))
    if layer_hints != None:
        root_symlinks["{}layer_hints".format(root_symlinks_prefix)] = layer_hints

    # Ship the sign_setting config file and signer plugin (if signing is active).
    sign_settings = [signing.config_info] if signing != None else []
    plugin_runfiles = add_sign_setting_symlinks(root_symlinks, sign_settings)

    pusher = ctx.actions.declare_file(ctx.label.name + ".exe")
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
        output_file = pusher,
        template_file = deploy_tool_info.launcher_template,
    )

    # Build environment for RunEnvironmentInfo
    environment = {
        "IMG_REAPI_ENDPOINT": ctx.attr._push_settings[PushSettingsInfo].remote_cache,
        "IMG_REAPI_INSTANCE_NAME": ctx.attr._push_settings[PushSettingsInfo].remote_instance_name,
        "IMG_CREDENTIAL_HELPER": ctx.attr._push_settings[PushSettingsInfo].credential_helper,
        "IMG_CREDENTIAL_HELPER_OCI_REGISTRY": ctx.attr._push_settings[PushSettingsInfo].credential_helper_oci_registry,
        "IMG_CREDENTIAL_HELPER_REMOTE_CACHE": ctx.attr._push_settings[PushSettingsInfo].credential_helper_remote_cache,
    }
    inherited_environment = [
        "IMG_REAPI_ENDPOINT",
        "IMG_REAPI_INSTANCE_NAME",
        "IMG_CREDENTIAL_HELPER",
        "IMG_CREDENTIAL_HELPER_OCI_REGISTRY",
        "IMG_CREDENTIAL_HELPER_REMOTE_CACHE",
        "IMG_AUTH_DEBUG",
        "DOCKER_CONFIG",
    ]

    # Add REGISTRY_AUTH_FILE if docker_config_path is set
    docker_config_path = ctx.attr._docker_config_path[BuildSettingInfo].value
    if docker_config_path:
        environment["REGISTRY_AUTH_FILE"] = docker_config_path

    direct_runfiles = [deploy_tool_info.img_deploy_exe, deploy_metadata]
    runfiles = ctx.runfiles(
        files = direct_runfiles,
        root_symlinks = root_symlinks,
    )
    for pr in plugin_runfiles:
        runfiles = runfiles.merge(pr)
    return [
        DefaultInfo(
            files = depset([pusher]),
            executable = pusher,
            runfiles = runfiles,
        ),
        OutputGroupInfo(
            deploy_manifest = depset([deploy_metadata]),
        ),
        RunEnvironmentInfo(
            environment = environment,
            inherited_environment = inherited_environment,
        ),
        DeployInfo(
            image = image_provider,
            deploy_manifest = deploy_metadata,
            layer_hints = layer_hints,
            include_layers = push_strategy == "eager",
            sign_settings = sign_settings,
            referrers = extract_referrers(ctx),
        ),
    ]

image_push = rule(
    implementation = _image_push_impl,
    doc = """Pushes container images to a registry.

This rule creates an executable target that uploads OCI images to container registries.
It supports multiple push strategies optimized for different use cases, from simple
uploads to advanced content-addressable storage integration.

Key features:
- **Multiple push strategies**: Choose between eager, lazy, CAS-based, or BES-integrated pushing
- **Template expansion**: Dynamic registry, repository, and tag values using build settings
- **Stamping support**: Include build information in image tags
- **Incremental uploads**: Skip blobs that already exist in the registry

The rule produces an executable that can be run with `bazel run`.

Example:

```python
load("@rules_img//img:push.bzl", "image_push")

# Simple push to Docker Hub
image_push(
    name = "push_app",
    image = ":my_app",
    registry = "index.docker.io",
    repository = "myorg/myapp",
    tag = "latest",
)

# Push multi-platform image with multiple tags
image_push(
    name = "push_multiarch",
    image = ":my_app_index",  # References an image_index
    registry = "gcr.io",
    repository = "my-project/my-app",
    tag_list = ["latest", "v1.0.0"],
)

# Dynamic push with build settings
image_push(
    name = "push_dynamic",
    image = ":my_app",
    registry = "{{.REGISTRY}}",
    repository = "{{.PROJECT}}/my-app",
    tag = "{{.VERSION}}",
    build_settings = {
        "REGISTRY": "//settings:registry",
        "PROJECT": "//settings:project",
        "VERSION": "//settings:version",
    },
)

# Push with stamping for unique tags
image_push(
    name = "push_stamped",
    image = ":my_app",
    registry = "index.docker.io",
    repository = "myorg/myapp",
    tag = "latest-{{.BUILD_TIMESTAMP}}",
    stamp = "force",
)

# Digest-only push (no tag)
image_push(
    name = "push_by_digest",
    image = ":my_app",
    registry = "gcr.io",
    repository = "my-project/my-app",
    # No tag specified - will push by digest only
)

# Push using a destination file (instead of registry/repository attributes)
image_push(
    name = "push_from_file",
    image = ":my_app",
    destination_file = ":push_destination.txt",
    tag = "latest",
)
```

Push strategies:
- **`eager`**: Materializes all layers next to push binary. Simple, correct, but may be inefficient.
- **`lazy`**: Layers are not stored locally. Missing layers are streamed from Bazel's remote cache.
- **`cas_registry`**: Uses content-addressable storage for extreme efficiency. Requires
  CAS-enabled infrastructure.
- **`bes`**: Image is pushed as side-effect of Build Event Stream upload. No "bazel run" command needed.
  Requires Build Event Service integration.

See [push strategies documentation](/docs/push-strategies.md) for detailed comparisons.

Runtime usage:
```bash
# Push to registry
bazel run //path/to:push_app

# The push command will output the image digest
```
""",
    attrs = dict(
        COMMON_PUSH_ATTRS,
        image = attr.label(
            doc = "Image to push. Should provide ImageManifestInfo or ImageIndexInfo.",
            mandatory = True,
        ),
        tool_cfg = attr.string(
            doc = """Configuration of the pusher executable platform.

Available options:
- **`host`** (default): Pusher executable matches the host platform.
- **`target`**: Pusher executable matches the target platform(s) specified via `--platforms`.
""",
            default = "host",
            values = ["host", "target"],
        ),
        deploy_tool = attr.label(
            doc = """Optional label of a deploy tool target providing `DeployToolInfo` (created with `img_deploy_tool` from `@rules_img//img:deploy_tool.bzl`). When set, overrides `tool_cfg`.""",
            providers = [DeployToolInfo],
        ),
        _deploy_tool = attr.label(
            default = default_deploy_tool,
            providers = [DeployToolInfo],
        ),
        _docker_config_path = attr.label(
            default = Label("//img/settings:docker_config_path"),
            providers = [BuildSettingInfo],
        ),
    ),
    executable = True,
    cfg = reset_platform_transition,
    toolchains = [
        launcher.finalizer_toolchain_type,
    ] + TOOLCHAINS,
)
