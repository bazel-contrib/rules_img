"""Repository rules for pulling container images."""

load("@bazel_skylib//lib:sets.bzl", "sets")
load("@pull_hub_repo//:defs.bzl", "tool_for_repository_os")
load("//img/private:manifest_media_type.bzl", "get_media_type")
load("//img/private/platforms:platforms.bzl", "has_constraint_setting")
load(
    ":download.bzl",
    _download_blob = "download_blob",
    _download_layers = "download_layers",
    _download_manifest = "download_manifest",
    _download_with_tool = "download_with_tool",
    _learn_digest_from_tag = "learn_digest_from_tag",
)
load(":registry.bzl", "get_registries")

def _map_os_arch_to_constraints(os_arch_pairs):
    """Map OS/architecture pairs to Bazel constraint labels.

    Args:
        os_arch_pairs: List of strings in format "os_arch" (e.g., ["linux_amd64", "darwin_arm64"])

    Returns:
        String representation of a select expression for target_compatible_with
    """
    if not os_arch_pairs:
        return "[]"

    # If there's only one platform, return its constraints directly
    if len(os_arch_pairs) == 1:
        return '["@rules_img//img/constraints:{}"]'.format(os_arch_pairs[0])

    # For multiple platforms, create a select expression
    select_dict = {}
    for os_arch in sorted(os_arch_pairs):
        select_dict['"@rules_img//img/constraints:{}"'.format(os_arch)] = "[]"
    select_dict['"//conditions:default"'] = '["{}"]'.format(str(Label("@platforms//:incompatible")))

    # Build the select expression string
    select_items = []
    for key, value in select_dict.items():
        select_items.append("        {}: {},".format(key, value))

    return "select({{\n{}\n    }})".format("\n".join(select_items))

def _pull_impl(rctx):
    """Pull an image from a registry and generate a BUILD file."""
    have_valid_digest = True
    if len(rctx.attr.digest) != 71:
        have_valid_digest = False
    elif not rctx.attr.digest.startswith("sha256:"):
        have_valid_digest = False

    # Learn the digest from tag if needed
    digest = rctx.attr.digest
    if not have_valid_digest and rctx.attr.unsafe_allow_tag_without_digest:
        # Do an extra roundtrip to learn the digest from the tag
        learned_digest = _learn_digest_from_tag(rctx, tag = rctx.attr.tag, downloader = rctx.attr.downloader)
        if learned_digest:
            digest = learned_digest
            have_valid_digest = True

    reference = digest if have_valid_digest else rctx.attr.tag
    if len(reference) == 0:
        fail("either digest or tag must be specified")

    if rctx.attr.downloader == "img_tool":
        # pre-download all files using the img tool
        # here if requested
        tool = tool_for_repository_os(rctx)
        tool_path = rctx.path(tool)
        _download_with_tool(
            rctx,
            tool_path = tool_path,
            reference = reference,
        )

    manifest_kwargs = dict(
        canonical_id = rctx.attr.repository + ((":" + rctx.attr.tag) if rctx.attr.tag else ("@" + digest)),
    )
    if rctx.attr.registry == "docker.io":
        print("Specified docker.io as registry. Did you mean \"index.docker.io\"?")  # buildifier: disable=print
    root_blob_info = _download_manifest(rctx, downloader = rctx.attr.downloader, reference = reference, **manifest_kwargs)
    data = {root_blob_info.digest: root_blob_info.data}
    root_blob = json.decode(root_blob_info.data)
    media_type = get_media_type(root_blob)

    manifests = []
    if media_type in [MEDIA_TYPE_INDEX, DOCKER_MANIFEST_LIST_V2]:
        is_index = True
        manifests = root_blob.get("manifests", [])
    elif media_type in [MEDIA_TYPE_MANIFEST, DOCKER_MANIFEST_V2]:
        is_index = False
        manifests = [{"mediaType": MEDIA_TYPE_MANIFEST, "digest": digest}]
    else:
        fail("invalid mediaType in manifest: {}".format(media_type))

    # TODO: switch to builtin set (requires Bazel 8+)
    # layer_digests = set()
    layer_digests = sets.make()
    platforms_set = sets.make()

    # download all manifests and configs
    for manifest_index in manifests:
        if manifest_index.get("mediaType") in [MEDIA_TYPE_INDEX, DOCKER_MANIFEST_LIST_V2]:
            # this is an index referenced by another index - we don't support nested indexes yet
            fail("image index referenced another index ({}). Nested indexes are not supported.".format(
                manifest_index["digest"],
            ))
        if not manifest_index.get("mediaType") in [MEDIA_TYPE_MANIFEST, DOCKER_MANIFEST_V2]:
            continue
        if is_index:
            manifest_info = _download_manifest(rctx, downloader = rctx.attr.downloader, reference = manifest_index["digest"])
            data[manifest_info.digest] = manifest_info.data

            # Extract platform from index manifest entry
            platform = manifest_index.get("platform", {})
            if platform:
                os = platform.get("os", "")
                arch = platform.get("architecture", "")
                if os and arch and has_constraint_setting(os, arch):
                    sets.insert(platforms_set, "{}_{}".format(os, arch))
        else:
            manifest_info = root_blob_info
        manifest = json.decode(manifest_info.data)
        config_info = _download_blob(rctx, downloader = rctx.attr.downloader, digest = manifest["config"]["digest"])
        data[config_info.digest] = config_info.data

        # Extract platform from config if not already found
        if not is_index:
            config = json.decode(config_info.data)
            os = config.get("os", "")
            arch = config.get("architecture", "")
            if os and arch and has_constraint_setting(os, arch):
                sets.insert(platforms_set, "{}_{}".format(os, arch))

        for layer in manifest.get("layers", []):
            sets.insert(layer_digests, layer["digest"])

    files = {
        digest: "//:blobs/{}".format(digest.replace("sha256:", "sha256/"))
        for digest in data.keys()
    }

    # materialize the blobs in the repository rule (if requested)
    if rctx.attr.layer_handling == "eager":
        files.update({
            layer.digest: "//:{}".format(layer.path)
            for layer in _download_layers(rctx, downloader = rctx.attr.downloader, digests = sets.to_list(layer_digests))
        })
    elif rctx.attr.layer_handling == "lazy":
        files.update({
            digest: "//:{}".format(digest.replace("sha256:", "sha256_"))
            for digest in sets.to_list(layer_digests)
        })

    registries = get_registries(rctx)

    name = getattr(rctx, "original_name", rctx.attr.name)
    if not hasattr(rctx, "original_name"):
        # we are on a Bazel version where `original_name` doesn't exist yet.
        # we need to unmangle the name.
        if "~" in name:
            # this is a Bazel 7 or earlier name:
            # _main~_repo_rules~distroless_cc
            name = name.split("~")[len(name.split("~")) - 1]
        elif "+" in name:
            # this is a Bazel 8 or later name:
            # _main+_repo_rules+distroless_cc
            name = name.split("+")[len(name.split("+")) - 1]

    loads = [
        ("@rules_img//img/private:import.bzl", "image_import"),
    ]
    maybe_lazy_layer_download = ""
    if rctx.attr.layer_handling == "lazy":
        # we need to load the download_layer rule if we are in lazy mode
        loads.append(("@rules_img//img/private:download_blobs.bzl", "download_blobs"))
        maybe_lazy_layer_download = """
download_blobs(
    name = "layers",
    digests = {layer_digests},
    registries = {registries},
    repository = {repository},
    tags = ["requires-network"],
)
""".format(
            layer_digests = json.encode_indent(
                sets.to_list(layer_digests),
                prefix = "    ",
                indent = "    ",
            ).replace("sha256:", "sha256_"),
            registries = json.encode_indent(
                registries,
                prefix = "    ",
                indent = "    ",
            ),
            repository = repr(rctx.attr.repository),
        )

    # Build target_compatible_with based on discovered platforms
    target_compatible_with = _map_os_arch_to_constraints(sets.to_list(platforms_set))

    # write out the files
    rctx.file(
        "BUILD.bazel",
        content = """# This file was generated by the pull repository rule.
{loads}
{maybe_lazy_layer_download}
image_import(
    name = "image",
    digest = {digest},
    data = {data},
    files = {files},
    registries = {registries},
    repository = {repository},
    tag = {tag},
    target_compatible_with = {target_compatible_with},
    visibility = ["//visibility:public"],
)

alias(
    name = {name},
    actual = ":image",
    visibility = ["//visibility:public"],
)
""".format(
            target_compatible_with = target_compatible_with,
            loads = "\n".join(
                ["load({}, {})".format(repr(path), repr(name)) for (path, name) in loads],
            ),
            maybe_lazy_layer_download = maybe_lazy_layer_download,
            name = repr(name),
            digest = repr(digest),
            data = json.encode_indent(
                data,
                prefix = "    ",
                indent = "    ",
            ),
            files = json.encode_indent(
                files,
                prefix = "    ",
                indent = "    ",
            ),
            registries = json.encode_indent(
                registries,
                prefix = "    ",
                indent = "    ",
            ),
            repository = repr(rctx.attr.repository),
            tag = repr(rctx.attr.tag) if rctx.attr.tag else "None",
        ),
    )
    if len(digest) > 0 and hasattr(rctx, "repo_metadata"):
        # allows participating in repo contents cache
        return rctx.repo_metadata(reproducible = True)

    # only to make buildifier happy
    return None

pull = repository_rule(
    implementation = _pull_impl,
    doc = """Pulls a container image from a registry using shallow pulling.

This repository rule implements shallow pulling - it only downloads the image manifest
and config, not the actual layer blobs. The layers are downloaded on-demand during
push operations or when explicitly needed. This significantly reduces bandwidth usage
and speeds up builds, especially for large base images.

Example usage in MODULE.bazel:
```starlark
pull = use_repo_rule("@rules_img//img:pull.bzl", "pull")

pull(
    name = "ubuntu",
    digest = "sha256:1e622c5f073b4f6bfad6632f2616c7f59ef256e96fe78bf6a595d1dc4376ac02",
    registry = "index.docker.io",
    repository = "library/ubuntu",
    tag = "24.04",
)
```

The `digest` parameter is recommended for reproducible builds. If omitted, the rule
will resolve the tag to a digest at fetch time and print a warning.
""",
    attrs = {
        "registry": attr.string(
            doc = """Primary registry to pull from (e.g., "index.docker.io", "gcr.io").

If not specified, defaults to Docker Hub. Can be overridden by entries in registries list.""",
        ),
        "registries": attr.string_list(
            doc = """List of mirror registries to try in order.

These registries will be tried in order before the primary registry. Useful for
corporate environments with registry mirrors or air-gapped setups.""",
        ),
        "repository": attr.string(
            mandatory = True,
            doc = """The image repository within the registry (e.g., "library/ubuntu", "my-project/my-image").

For Docker Hub, official images use "library/" prefix (e.g., "library/ubuntu").""",
        ),
        "tag": attr.string(
            doc = """The image tag to pull (e.g., "latest", "24.04", "v1.2.3").

While required, it's recommended to also specify a digest for reproducible builds.""",
        ),
        "digest": attr.string(
            doc = """The image digest for reproducible pulls (e.g., "sha256:abc123...").

When specified, the image is pulled by digest instead of tag, ensuring reproducible
builds. The digest must be a full SHA256 digest starting with "sha256:".""",
        ),
        "layer_handling": attr.string(
            default = "shallow",
            values = ["shallow", "eager", "lazy"],
            doc = """Strategy for handling image layers.

This attribute controls when and how layer data is fetched from the registry.

**Available strategies:**

* **`shallow`** (default): Layer data is fetched only if needed during push operations,
  but is not available during the build. This is the most efficient option for images
  that are only used as base images for pushing.

* **`eager`**: Layer data is fetched in the repository rule and is always available.
  This ensures layers are accessible in build actions but is inefficient as all layers
  are downloaded regardless of whether they're needed. Use this for base images that
  need to be read or inspected during the build.

* **`lazy`**: Layer data is downloaded in a build action when requested. This provides
  access to layers during builds while avoiding unnecessary downloads, but requires
  network access during the build phase. **EXPERIMENTAL:** Use at your own risk.
""",
        ),
        "downloader": attr.string(
            default = "img_tool",
            values = ["img_tool", "bazel"],
            doc = """The tool to use for downloading manifests and blobs.

**Available options:**

* **`img_tool`** (default): Uses the `img` tool for all downloads.

* **`bazel`**: Uses Bazel's native HTTP capabilities for downloading manifests and blobs.
""",
        ),
        "unsafe_allow_tag_without_digest": attr.bool(
            default = False,
            doc = """Allow pulling by tag without specifying a digest.

**WARNING:** This is not recommended for reproducible builds as tags can be moved
to point to different image versions. Only use this when you're managing reproducibility
through other means (e.g., content-based tags).

When enabled, the rule will resolve the tag to a digest at fetch time and use that
digest, but will not fail if no digest is explicitly provided.""",
        ),
    },
)

MEDIA_TYPE_INDEX = "application/vnd.oci.image.index.v1+json"
DOCKER_MANIFEST_LIST_V2 = "application/vnd.docker.distribution.manifest.list.v2+json"
MEDIA_TYPE_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
DOCKER_MANIFEST_V2 = "application/vnd.docker.distribution.manifest.v2+json"
