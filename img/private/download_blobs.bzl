"""Build rules for downloading container image blobs during build time.

This module provides the download_blobs rule which enables lazy downloading of image layers
as part of build actions rather than repository rules. This is useful for scenarios where
layer data needs to be available during builds but you want to avoid downloading all layers
upfront during repository fetching.
"""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:transitions.bzl", "reset_platform_transition")
load("//img/private/repository_rules:registry.bzl", "get_sources_list")

def _download_blob(ctx, output):
    """Download a layer from a container registry."""
    if not output.basename.startswith("sha256_"):
        fail("invalid digest: {}".format(output.basename))
    digest = output.basename.replace("sha256_", "sha256:")

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo

    # Convert sources dict to list of "repository@registry" strings
    sources_list = get_sources_list(ctx.attr.sources)

    # Only set REGISTRY_AUTH_FILE if docker_config_path is non-empty
    docker_config_path = ctx.attr._docker_config_path[BuildSettingInfo].value
    env = {}
    if docker_config_path:
        env["REGISTRY_AUTH_FILE"] = docker_config_path

    ctx.actions.run(
        outputs = [output],
        executable = img_toolchain_info.tool_exe,
        use_default_shell_env = True,
        arguments = [
            "download-blob",
            "--digest",
            digest,
            "--output",
            output.path,
        ] + [
            "--source={}".format(source)
            for source in sources_list
        ],
        env = env,
        mnemonic = "DownloadBlob",
    )

def _download_blobs_impl(ctx):
    """Downloads blobs from a container registry in a build action."""
    if len(ctx.outputs.digests) == 0:
        fail("need at least one digest to pull from")

    for output in ctx.outputs.digests:
        _download_blob(ctx, output = output)

    return [
        DefaultInfo(files = depset(ctx.outputs.digests)),
        OutputGroupInfo(
            layer = depset(ctx.outputs.digests),
            # TODO...
            # metadata = depset([metadata_out]),
        ),
    ]

download_blobs = rule(
    implementation = _download_blobs_impl,
    attrs = {
        "digests": attr.output_list(
            doc = "List of digests to download.",
            mandatory = True,
        ),
        "sources": attr.string_list_dict(
            mandatory = True,
            doc = """Mapping of image repositories to lists of registries that serve them.

Each entry specifies a repository path and the registries that can serve it:
- Key: The image repository (e.g., "library/ubuntu", "my-project/my-image")
- Value: List of registries that serve this repository

All repository@registry combinations will be tried (in random order for load distribution).

If a registry list is empty, it defaults to Docker Hub (index.docker.io).""",
        ),
        "_docker_config_path": attr.label(
            default = Label("//img/settings:docker_config_path"),
            providers = [BuildSettingInfo],
        ),
    },
    toolchains = TOOLCHAINS,
    cfg = reset_platform_transition,
)
