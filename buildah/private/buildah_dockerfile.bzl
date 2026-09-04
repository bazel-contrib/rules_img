"""Building image from Dockerfile using buildah.
Delta-only layer export: only newly built layers are extracted, base layers are passed through.
Uses an in-process registry to receive push output and proxy upstream layers for chained builds.
"""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("@rules_img//img:media_types.bzl", "ZSTD_LAYER")
load("@rules_img//img:providers.bzl", "ImageIndexInfo", "ImageManifestInfo", "PullInfo", "SingleLayerInfo")
load("@rules_img//img:sparse_oci_layout.bzl", "build_sparse_oci_layout_for_manifest")

TOOLCHAINS = [str(Label("@rules_img//img:toolchain_type"))]

BuildahBaseInfo = provider(
    doc = "Threads upstream registry ref and local delta layers through a buildah_dockerfile chain.",
    fields = {
        "upstream_ref": "Full registry/repository string for display (e.g. registry.example.com/repo).",
        "upstream_registry": "Registry host (e.g. registry.example.com).",
        "upstream_repository": "Repository path (e.g. library/ubuntu).",
        "upstream_digest": "Manifest digest of the upstream image, used to fetch base blobs by digest.",
        "local_layers": "List of SingleLayerInfo for delta layers produced by intermediate buildah_dockerfile targets.",
        "local_layer_cache_repos": "List of remote cache repos (registry/repository or empty string) aligned with local_layers, naming where each layer was cache-pushed.",
    },
)

def _resolve_base_manifest_info(base, architecture, os):
    if ImageManifestInfo in base:
        return base[ImageManifestInfo]

    if ImageIndexInfo in base:
        index = base[ImageIndexInfo]
        for m in index.manifests:
            if m.architecture == architecture and m.os == os:
                return m

        if len(index.manifests) == 1:
            return index.manifests[0]

        fail("no manifest for {}/{} in ImageIndexInfo".format(architecture, os))
    return None

def _buildah_dockerfile_impl(ctx):
    pull_info = None
    base_manifest_info = None
    base_buildah_info = None
    base_layers = []

    if ctx.attr.base:
        if PullInfo in ctx.attr.base:
            pull_info = ctx.attr.base[PullInfo]
        if BuildahBaseInfo in ctx.attr.base:
            base_buildah_info = ctx.attr.base[BuildahBaseInfo]
        base_manifest_info = _resolve_base_manifest_info(ctx.attr.base, ctx.attr.architecture, ctx.attr.os)
        if base_manifest_info:
            base_layers = base_manifest_info.layers

    has_base = pull_info != None or base_buildah_info != None
    new_layer_count = ctx.attr.layer_count

    output_manifest = ctx.actions.declare_file("{}_image_manifest.json".format(ctx.attr.name))
    output_config = ctx.actions.declare_file("{}_image_config.json".format(ctx.attr.name))
    output_descriptor = ctx.actions.declare_file("{}_image_descriptor.json".format(ctx.attr.name))
    output_digest = ctx.actions.declare_file("{}_digest".format(ctx.attr.name))
    build_log = ctx.actions.declare_file("{}.build.log".format(ctx.attr.name))

    output_blobs = [
        ctx.actions.declare_file("{}_layer_blob_{}.tzst".format(ctx.attr.name, i))
        for i in range(new_layer_count)
    ]
    output_metadata = [
        ctx.actions.declare_file("{}_metadata_{}.json".format(ctx.attr.name, i))
        for i in range(new_layer_count)
    ]

    staging_dir = ctx.actions.declare_directory("{}_staging".format(ctx.attr.name))

    args = ctx.actions.args()
    args.add("--dockerfile", ctx.file.dockerfile.path)
    args.add("--architecture", ctx.attr.architecture)
    args.add("--os", ctx.attr.os)
    args.add("--build-log", build_log.path)
    args.add("--staging-dir", staging_dir.path)
    args.add("--output-manifest", output_manifest.path)
    args.add("--output-config", output_config.path)
    args.add("--output-descriptor", output_descriptor.path)
    args.add("--output-digest", output_digest.path)

    for t in ctx.attr.context:
        for f in t.files.to_list():
            args.add("--context-file", "{}={}".format(f.path, f.short_path))

    for i in range(new_layer_count):
        args.add("--output-layer-blob", output_blobs[i].path)
        args.add("--output-layer-metadata", output_metadata[i].path)

    remote_cache = ctx.attr.remote_cache or ctx.attr._default_remote_cache[BuildSettingInfo].value
    if remote_cache:
        args.add("--cache-from", remote_cache)
        args.add("--cache-to", remote_cache)

    if ctx.attr.estargz:
        args.add("--estargz")

    for host in ctx.attr.add_hosts:
        args.add("--add-host", host)

    base_inputs = []

    if base_buildah_info:
        if base_buildah_info.upstream_repository:
            args.add("--upstream-registry", base_buildah_info.upstream_registry)
            args.add("--upstream-repository", base_buildah_info.upstream_repository)
            args.add("--upstream-digest", base_buildah_info.upstream_digest)
    elif pull_info:
        args.add("--upstream-registry", pull_info.registries[0])
        args.add("--upstream-repository", pull_info.repository)
        args.add("--upstream-digest", pull_info.digest)

    if base_manifest_info:
        args.add("--base-manifest", base_manifest_info.manifest.path)
        args.add("--base-config", base_manifest_info.config.path)
        base_inputs.extend([base_manifest_info.manifest, base_manifest_info.config])
        for layer in base_layers:
            args.add("--base-layer-metadata", layer.metadata.path)
            base_inputs.append(layer.metadata)

    if base_buildah_info:
        base_cache_repos = base_buildah_info.local_layer_cache_repos
        for i, layer in enumerate(base_buildah_info.local_layers):
            args.add("--local-layer-blob", layer.blob.path)
            args.add("--local-layer-metadata", layer.metadata.path)
            args.add("--local-layer-cache-repo", base_cache_repos[i] if i < len(base_cache_repos) else "")
            base_inputs.extend([layer.blob, layer.metadata])
    elif base_manifest_info:
        for layer in base_manifest_info.layers:
            if layer.blob:
                args.add("--local-layer-blob", layer.blob.path)
                args.add("--local-layer-metadata", layer.metadata.path)
                args.add("--local-layer-cache-repo", "")
                base_inputs.extend([layer.blob, layer.metadata])

    context_files = []
    for t in ctx.attr.context:
        context_files += list(t.files.to_list())

    outputs = [output_manifest, output_config, output_descriptor, output_digest, build_log, staging_dir] + output_blobs + output_metadata

    if ctx.attr._verbose[BuildSettingInfo].value:
        args.add("--verbose")

    if ctx.attr._debug[BuildSettingInfo].value:
        args.add("--debug")

    ctx.actions.run(
        executable = ctx.executable._buildah_tool,
        arguments = [args],
        inputs = [ctx.file.dockerfile] + context_files + base_inputs,
        outputs = outputs,
        mnemonic = "BuildahTool",
        progress_message = "Building {} (buildah)".format(ctx.file.dockerfile.short_path),
        execution_requirements = {
            "no-remote-exec": "1",
            "no-sandbox": "1",
        },
    )

    output_layer_infos = [
        SingleLayerInfo(
            blob = output_blobs[i],
            metadata = output_metadata[i],
            media_type = ZSTD_LAYER,
            estargz = ctx.attr.estargz,
            compact_stream = None,
            layer_input_files = None,
            layer_input_files_cas = None,
            sources = [],
            mtree = None,
            ztoc = None,
        )
        for i in range(new_layer_count)
    ]

    if has_base:
        all_layers = list(base_layers) + output_layer_infos
    else:
        all_layers = output_layer_infos

    all_local_layers = []
    all_local_cache_repos = []
    if base_buildah_info:
        all_local_layers = list(base_buildah_info.local_layers)
        all_local_cache_repos = list(base_buildah_info.local_layer_cache_repos)
    all_local_layers += output_layer_infos
    all_local_cache_repos += [remote_cache] * new_layer_count

    providers = [
        DefaultInfo(files = depset([output_manifest, output_config, build_log])),
        OutputGroupInfo(
            descriptor = depset([output_descriptor]),
            digest = depset([output_digest]),
        ),
        ImageManifestInfo(
            descriptor = output_descriptor,
            manifest = output_manifest,
            config = output_config,
            structured_config = {},
            architecture = ctx.attr.architecture,
            os = ctx.attr.os,
            variant = "",
            layers = all_layers,
            mtree = None,
            sparse_oci_layout = build_sparse_oci_layout_for_manifest(ctx, output_manifest, output_config, all_layers),
        ),
        BuildahBaseInfo(
            upstream_registry = pull_info.registries[0] if pull_info else (base_buildah_info.upstream_registry if base_buildah_info else ""),
            upstream_repository = pull_info.repository if pull_info else (base_buildah_info.upstream_repository if base_buildah_info else ""),
            upstream_digest = pull_info.digest if pull_info else (base_buildah_info.upstream_digest if base_buildah_info else ""),
            local_layers = all_local_layers,
            local_layer_cache_repos = all_local_cache_repos,
        ),
    ]

    if pull_info:
        providers.append(pull_info)

    return providers

buildah_dockerfile = rule(
    implementation = _buildah_dockerfile_impl,
    attrs = {
        "dockerfile": attr.label(allow_single_file = True, mandatory = True),
        "context": attr.label_list(allow_files = True),
        "layer_count": attr.int(mandatory = True, doc = "Number of layers in resulting image minus number of layers in base image (from base attr)"),
        "remote_cache": attr.string(),
        "estargz": attr.bool(default = False, doc = "Produce zstd:chunked (estargz) layers for layer-rsync reuse."),
        "add_hosts": attr.string_list(doc = "Extra host-to-IP mappings passed to buildah as --add-host (host:ip)."),
        "architecture": attr.string(default = "amd64"),
        "os": attr.string(default = "linux"),
        "base": attr.label(),
        "_buildah_tool": attr.label(
            default = Label("//cmd/buildah_tool"),
            executable = True,
            cfg = "exec",
        ),
        "_verbose": attr.label(
            default = Label("//settings:verbose"),
            providers = [BuildSettingInfo],
        ),
        "_debug": attr.label(
            default = Label("//settings:debug"),
            providers = [BuildSettingInfo],
        ),
        "_default_remote_cache": attr.label(
            default = Label("//settings:default_remote_cache"),
            providers = [BuildSettingInfo],
        ),
    },
    toolchains = TOOLCHAINS,
    provides = [ImageManifestInfo],
)
