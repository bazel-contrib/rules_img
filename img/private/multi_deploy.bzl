"""Multi deploy rule for deploying multiple operations as a unified command."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@hermetic_launcher//launcher:lib.bzl", "launcher")
load("@platforms//host:constraints.bzl", "HOST_CONSTRAINTS")
load("//img/private:root_symlinks.bzl", "calculate_root_symlinks")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:transitions.bzl", "reset_platform_transition")
load("//img/private/providers:deploy_info.bzl", "DeployInfo")
load("//img/private/providers:load_settings_info.bzl", "LoadSettingsInfo")
load("//img/private/providers:push_settings_info.bzl", "PushSettingsInfo")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

def _multi_deploy_strategy(ctx, operation_type):
    """Determine the strategy to use based on the settings and operation type."""
    if operation_type == "push":
        push_settings = ctx.attr._push_settings[PushSettingsInfo]
        strategy = ctx.attr.push_strategy
        if strategy == "auto":
            strategy = push_settings.strategy
        return strategy
    elif operation_type == "load":
        load_settings = ctx.attr._load_settings[LoadSettingsInfo]
        strategy = ctx.attr.load_strategy
        if strategy == "auto":
            strategy = load_settings.strategy
        return strategy
    else:
        fail("Unknown operation type: {}".format(operation_type))

def _compute_multi_deploy_metadata(*, ctx):
    """Compute the merged deploy metadata from all operations."""
    inputs = []
    deploy_manifests = []

    # Collect all deploy manifests from operations
    for operation in ctx.attr.operations:
        deploy_info = operation[DeployInfo]
        deploy_manifests.append(deploy_info.deploy_manifest)
        inputs.append(deploy_info.deploy_manifest)

    # Create the merge command
    args = ctx.actions.args()
    args.add("deploy-merge")
    args.add("--push-strategy", _multi_deploy_strategy(ctx, "push"))
    args.add("--load-strategy", _multi_deploy_strategy(ctx, "load"))

    # Add input deploy manifest files
    for manifest in deploy_manifests:
        args.add(manifest.path)

    # Output file
    metadata_out = ctx.actions.declare_file(ctx.label.name + ".json")
    args.add(metadata_out.path)

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        inputs = inputs,
        outputs = [metadata_out],
        executable = img_toolchain_info.tool_exe,
        arguments = [args],
        mnemonic = "MultiDeployMerge",
    )
    return metadata_out

def _collect_all_image_providers(ctx):
    """Collect all image providers from operations to build root symlinks."""
    images = []
    for operation in ctx.attr.operations:
        deploy_info = operation[DeployInfo]
        if hasattr(deploy_info.image, "manifests"):
            # It's an index
            images.append(dict(
                index_info = deploy_info.image,
                manifest_info = None,
            ))
        else:
            # It's a manifest
            images.append(dict(
                index_info = None,
                manifest_info = deploy_info.image,
            ))
    return images

def _multi_deploy_impl(ctx):
    """Implementation of the multi_deploy rule."""
    if not ctx.attr.operations:
        fail("operations attribute cannot be empty")

    # Create the executable
    deployer = ctx.actions.declare_file(ctx.label.name + ".exe")
    img_toolchain_info = ctx.exec_groups["host"].toolchains[TOOLCHAIN].imgtoolchaininfo
    embedded_args, transformed_args = launcher.args_from_entrypoint(executable_file = img_toolchain_info.tool_exe)
    launcher.compile_stub(
        ctx = ctx,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
        output_file = deployer,
        cfg = "exec",
        template_exec_group = "host",
    )

    # Merge all deploy manifests
    dispatch_json = _compute_multi_deploy_metadata(ctx = ctx)

    # Collect all image providers for root symlinks
    images = _collect_all_image_providers(ctx)

    # Build root symlinks including all layers from all operations
    # We need to include layers for strategies that require them
    include_layers = (
        _multi_deploy_strategy(ctx, "push") == "eager" or
        _multi_deploy_strategy(ctx, "load") == "eager"
    )

    root_symlinks = {}

    # Add symlinks for all deploy commands
    for (i, image) in enumerate(images):
        symlinks = calculate_root_symlinks(
            index_info = image["index_info"],
            manifest_info = image["manifest_info"],
            include_layers = include_layers,
            operation_index = i,
        )
        root_symlinks.update(symlinks)

    # Add the dispatch JSON
    root_symlinks["dispatch.json"] = dispatch_json

    # Merge environment settings from push and load
    environment = {}
    inherited_environment = ["DOCKER_CONFIG"]

    push_settings = ctx.attr._push_settings[PushSettingsInfo]
    load_settings = ctx.attr._load_settings[LoadSettingsInfo]

    if push_settings.remote_cache or load_settings.remote_cache:
        environment["IMG_REAPI_ENDPOINT"] = push_settings.remote_cache or load_settings.remote_cache
        inherited_environment.append("IMG_REAPI_ENDPOINT")

    if push_settings.credential_helper or load_settings.credential_helper:
        environment["IMG_CREDENTIAL_HELPER"] = push_settings.credential_helper or load_settings.credential_helper
        inherited_environment.append("IMG_CREDENTIAL_HELPER")

    # Add REGISTRY_AUTH_FILE if docker_config_path is set
    docker_config_path = ctx.attr._docker_config_path[BuildSettingInfo].value
    if docker_config_path:
        environment["REGISTRY_AUTH_FILE"] = docker_config_path

    return [
        DefaultInfo(
            files = depset([dispatch_json]),
            executable = deployer,
            runfiles = ctx.runfiles(files = [img_toolchain_info.tool_exe], root_symlinks = root_symlinks),
        ),
        RunEnvironmentInfo(
            environment = environment,
            inherited_environment = inherited_environment,
        ),
    ]

multi_deploy = rule(
    implementation = _multi_deploy_impl,
    doc = """Merges multiple deploy operations into a single unified deployment command.

This rule takes multiple operations (typically from image_push or image_load rules)
that provide DeployInfo and merges them into a single command that can deploy all
operations in parallel. This is useful for scenarios where you need to push and/or
load multiple related images as a coordinated deployment.

The rule produces an executable that can be run with `bazel run`.

Example:

```python
load("@rules_img//img:push.bzl", "image_push")
load("@rules_img//img:load.bzl", "image_load")
load("@rules_img//img:multi_deploy.bzl", "multi_deploy")

# Individual operations
image_push(
    name = "push_frontend",
    image = ":frontend",
    registry = "gcr.io",
    repository = "my-project/frontend",
    tag = "latest",
)

image_push(
    name = "push_backend",
    image = ":backend",
    registry = "gcr.io",
    repository = "my-project/backend",
    tag = "latest",
)

image_load(
    name = "load_database",
    image = ":database",
    tag = "my-database:latest",
)

# Unified deployment
multi_deploy(
    name = "deploy_all",
    operations = [
        ":push_frontend",
        ":push_backend",
        ":load_database",
    ],
    push_strategy = "lazy",
    load_strategy = "eager",
)
```

Runtime usage:
```bash
# Deploy all operations together
bazel run //path/to:deploy_all
```

The deploy-merge subcommand will execute all push and load operations in sequence,
allowing for coordinated deployment of related container images.
""",
    attrs = {
        "operations": attr.label_list(
            doc = """List of operations to deploy together.

Each operation must provide DeployInfo (typically from image_push or image_load rules).
All operations will be merged and executed in the order specified.
""",
            mandatory = True,
            providers = [DeployInfo],
        ),
        "push_strategy": attr.string(
            doc = """Push strategy to use for all push operations in the deployment.

See [push strategies documentation](/docs/push-strategies.md) for detailed information.
""",
            default = "auto",
            values = ["auto", "eager", "lazy", "cas_registry", "bes"],
        ),
        "load_strategy": attr.string(
            doc = """Load strategy to use for all load operations in the deployment.

Available strategies:
- **`auto`** (default): Uses the global default load strategy
- **`eager`**: Downloads all layers during the build phase
- **`lazy`**: Downloads layers only when needed during the load operation
""",
            default = "auto",
            values = ["auto", "eager", "lazy"],
        ),
        "_push_settings": attr.label(
            default = Label("//img/private/settings:push"),
            providers = [PushSettingsInfo],
        ),
        "_load_settings": attr.label(
            default = Label("//img/private/settings:load"),
            providers = [LoadSettingsInfo],
        ),
        "_stamp_settings": attr.label(
            default = Label("//img/private/settings:stamp"),
            providers = [StampSettingInfo],
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
    toolchains = [launcher.finalizer_toolchain_type] + TOOLCHAINS,
)
