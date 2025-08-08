"""Build rules for downloading container image blobs during build time.

This module provides the download_blobs rule which enables lazy downloading of image layers
as part of build actions rather than repository rules. This is useful for scenarios where
layer data needs to be available during builds but you want to avoid downloading all layers
upfront during repository fetching.
"""

load("//img/private/common:build.bzl", "TOOLCHAIN", "TOOLCHAINS")
load("//img/private/common:transitions.bzl", "reset_platform_transition")

def _download_blob(ctx, output):
    """Download a layer from a container registry."""
    if not output.basename.startswith("sha256_"):
        fail("invalid digest: {}".format(output.basename))
    digest = output.basename.replace("sha256_", "sha256:")

    img_toolchain_info = ctx.toolchains[TOOLCHAIN].imgtoolchaininfo
    ctx.actions.run(
        outputs = [output],
        executable = img_toolchain_info.tool_exe,
        arguments = [
            "download-blob",
            "--digest",
            digest,
            "--repository",
            ctx.attr.repository,
            "--output",
            output.path,
        ] + [
            "--registry={}".format(r)
            for r in ctx.attr.registries
        ],
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
        "registries": attr.string_list(
            doc = "List of registry mirrors used to pull the image.",
        ),
        "repository": attr.string(
            doc = "Repository name of the image.",
        ),
    },
    toolchains = TOOLCHAINS,
    cfg = reset_platform_transition,
)
