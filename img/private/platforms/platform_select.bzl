"""Rule that picks the manifest matching the target platform out of a set of manifests."""

load("//img/private/config:defs.bzl", "TargetPlatformInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:pull_info.bzl", "PullInfo")
load(":match.bzl", "match_manifest", "no_match_message")

def _image_platform_select_impl(ctx):
    target_platform = ctx.attr._os_cpu[TargetPlatformInfo]
    infos = [manifest[ImageManifestInfo] for manifest in ctx.attr.manifests]
    match = match_manifest(
        infos,
        target_platform.os,
        target_platform.cpu,
        target_platform.variant,
    )
    if match == None:
        fail(no_match_message(
            target_platform.os,
            target_platform.cpu,
            target_platform.variant,
        ))

    selected = None
    for i in range(len(infos)):
        if infos[i] == match:
            selected = ctx.attr.manifests[i]
            break

    providers = [
        DefaultInfo(files = selected[DefaultInfo].files),
        match,
    ]
    if PullInfo in selected:
        providers.append(selected[PullInfo])
    return providers

image_platform_select = rule(
    implementation = _image_platform_select_impl,
    doc = """Forwards the one of `manifests` that matches the target platform.

Pulled images expose a single manifest per target platform through a `select()` on the
os/architecture of the target platform. That is not enough when an image index has several
manifests for the same os/architecture (for example `linux/arm/v6` and `linux/arm/v7`), because
Bazel constraints don't model containerd's variant fallback. This rule resolves those few
candidates in the analysis phase instead, using the same matching logic as the `base` attribute
of `image_manifest`.""",
    attrs = {
        "manifests": attr.label_list(
            mandatory = True,
            providers = [ImageManifestInfo],
            doc = "Candidate manifests. All of them share an os/architecture and differ in variant.",
        ),
        "_os_cpu": attr.label(
            default = Label("//img/private/config:target_os_cpu"),
            providers = [TargetPlatformInfo],
        ),
    },
)
