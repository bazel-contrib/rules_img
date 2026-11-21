"""Repository rules for pulling container images."""

load("@bazel_skylib//lib:sets.bzl", "sets")
load("@img_toolchain//:defs.bzl", "tool_for_repository_os")
load("//img/private:manifest_media_type.bzl", "get_media_type")
load("//img/private/platforms:constraints.bzl", "map_os_arch_to_constraints")
load("//img/private/platforms:matching.bzl", "parse_platform_spec", "select_index_children")
load("//img/private/platforms:platforms.bzl", "has_constraint_setting")
load(
    ":download.bzl",
    _download_blob = "download_blob",
    _download_layers = "download_layers",
    _download_manifest_rctx = "download_manifest_rctx",
    _download_with_tool = "download_with_tool",
    _lazy_download_auth_attr = "lazy_download_auth_attr",
    _learn_digest_from_tag = "learn_digest_from_tag",
    _resolve_docker_config_path = "resolve_docker_config_path",
)
load(":referrers.bzl", "build_referrer_artifacts", "check_digests", "discover_referrers_with_tool", "flatten_discovered", "referrer_blob_digests", "referrer_subject", "subject_descriptors")
load(":registry.bzl", "get_registries", "image_purl")

def _pull_impl(rctx):
    """Pull an image from a registry and generate a BUILD file."""

    # Parse the platform filter before any network access, so a typo fails fast.
    platform_specs = [parse_platform_spec(spec) for spec in rctx.attr.platforms]
    check_digests(rctx.attr.referrer_digests, "referrer_digests of {}".format(rctx.attr.repository))
    _resolve_docker_config_path(rctx, rctx.attr.docker_config_path, rctx.attr.config)

    have_valid_digest = True
    if len(rctx.attr.digest) != 71:
        have_valid_digest = False
    elif not rctx.attr.digest.startswith("sha256:"):
        have_valid_digest = False

    # Build sources from legacy attrs for download functions
    registries = get_registries(rctx)
    sources = {rctx.attr.repository: registries}

    # Learn the digest from tag if needed
    digest = rctx.attr.digest
    if not have_valid_digest and rctx.attr.unsafe_allow_tag_without_digest:
        # Do an extra roundtrip to learn the digest from the tag
        learned_digest = _learn_digest_from_tag(rctx, tag = rctx.attr.tag, downloader = rctx.attr.downloader, sources = sources)
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
            platforms = rctx.attr.platforms,
            referrers = rctx.attr.referrer_digests,
            referrer_artifact_types = rctx.attr.referrer_artifact_types,
            discovered_referrers = _DISCOVERED_REFERRERS if rctx.attr.referrer_artifact_types else None,
        )

    manifest_kwargs = dict(
        canonical_id = rctx.attr.repository + ((":" + rctx.attr.tag) if rctx.attr.tag else ("@" + digest)),
    )
    if rctx.attr.registry == "docker.io":
        print("Specified docker.io as registry. Did you mean \"index.docker.io\"?")  # buildifier: disable=print
    root_blob_info = _download_manifest_rctx(rctx, downloader = rctx.attr.downloader, reference = reference, **manifest_kwargs)
    data = {root_blob_info.digest: root_blob_info.data}
    root_blob = json.decode(root_blob_info.data)
    media_type = get_media_type(root_blob)

    # Children of the index that are not downloaded, either because the platform filter
    # rejected them or because they are not image manifests at all. The index blob is used
    # verbatim (the digest pin refers to it), so it keeps referring to them and image_import
    # has to be told to skip them instead of failing on the missing blob.
    omitted_manifests = []

    manifests = []
    if media_type in [MEDIA_TYPE_INDEX, DOCKER_MANIFEST_LIST_V2]:
        is_index = True

        # Triage the whole index before downloading anything, so a spec that matches no
        # child is reported without having spent requests on the specs that did match.
        selection = select_index_children(root_blob.get("manifests", []), platform_specs)
        if len(selection.nested) > 0:
            # this is an index referenced by another index - we don't support nested indexes yet
            fail("image index referenced another index ({}). Nested indexes are not supported.".format(
                selection.nested[0],
            ))
        if len(selection.unmatched) > 0:
            fail("""image index {reference} of {repository} has no manifest for platform(s) {unmatched}.
Available platforms: {available}""".format(
                reference = reference,
                repository = rctx.attr.repository,
                unmatched = ", ".join(['"{}"'.format(spec) for spec in selection.unmatched]),
                available = ", ".join(selection.available),
            ))
        manifests = selection.selected
        omitted_manifests = selection.omitted
    elif media_type in [MEDIA_TYPE_MANIFEST, DOCKER_MANIFEST_V2]:
        is_index = False
        manifests = [{"mediaType": MEDIA_TYPE_MANIFEST, "digest": digest}]
    else:
        fail("invalid mediaType in manifest: {}".format(media_type))

    # TODO: switch to builtin set (requires Bazel 8+)
    # layer_digests = set()
    layer_digests = sets.make()
    platforms_set = sets.make()

    # download the selected manifests and configs
    for manifest_index in manifests:
        if is_index:
            manifest_info = _download_manifest_rctx(rctx, downloader = rctx.attr.downloader, reference = manifest_index["digest"])
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
        config_info = _download_blob(rctx, downloader = rctx.attr.downloader, digest = manifest["config"]["digest"], sources = sources)
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

    # Referrers apply to the root or to one of its children, as named by their own
    # `subject`. They are fetched in full whatever the layer handling: an SBOM or a
    # signature is read during the build even if the image's layers are not.
    image_name = "{}@{}".format(rctx.attr.repository, digest)
    subjects = subject_descriptors(digest, root_blob_info.data)
    referrer_digests = list(rctx.attr.referrer_digests)

    # Discover referrers by artifact type, for the root and the downloaded children.
    if rctx.attr.referrer_artifact_types:
        if rctx.attr.downloader == "img_tool":
            discovered = json.decode(rctx.read(_DISCOVERED_REFERRERS))
            rctx.delete(_DISCOVERED_REFERRERS)
        else:
            discovered = discover_referrers_with_tool(
                rctx,
                subjects = [digest] + ([child["digest"] for child in manifests] if is_index else []),
                artifact_types = rctx.attr.referrer_artifact_types,
                sources = sources,
            )
        for found in discovered.values():
            for referrer_digest in flatten_discovered(found):
                if referrer_digest not in referrer_digests:
                    referrer_digests.append(referrer_digest)

    referrers = []
    referrer_blobs = {}
    for referrer_digest in referrer_digests:
        if referrer_digest in data:
            continue
        referrer_info = _download_manifest_rctx(rctx, downloader = rctx.attr.downloader, reference = referrer_digest)
        referrer_manifest = json.decode(referrer_info.data)
        referrer_subject(referrer_digest, referrer_manifest, subjects, image_name)
        data[referrer_digest] = referrer_info.data
        referrers.append(referrer_digest)
        for blob_digest in referrer_blob_digests(referrer_manifest):
            referrer_blobs[blob_digest] = True
    for blob in _download_layers(rctx, downloader = rctx.attr.downloader, digests = referrer_blobs.keys(), sources = sources):
        referrer_blobs[blob.digest] = "//:{}".format(blob.path)

    # Artifacts from other sources, built locally into blobs/ and bound to their subject.
    local_referrers = []
    for artifact in build_referrer_artifacts(rctx, specs = rctx.attr.referrer_artifacts, subjects = subjects, default_subject = digest):
        if artifact.digest in data:
            continue
        artifact_data = rctx.read("blobs/" + artifact.digest.replace("sha256:", "sha256/"))
        data[artifact.digest] = artifact_data
        local_referrers.append(artifact.digest)
        for blob_digest in referrer_blob_digests(json.decode(artifact_data)):
            referrer_blobs[blob_digest] = "//:blobs/{}".format(blob_digest.replace("sha256:", "sha256/"))

    files = {
        digest: "//:blobs/{}".format(digest.replace("sha256:", "sha256/"))
        for digest in data.keys()
    }

    # materialize the blobs in the repository rule (if requested)
    if rctx.attr.layer_handling == "eager":
        files.update({
            layer.digest: "//:{}".format(layer.path)
            for layer in _download_layers(rctx, downloader = rctx.attr.downloader, digests = sets.to_list(layer_digests), sources = sources)
        })
    elif rctx.attr.layer_handling == "lazy":
        files.update({
            digest: "//:{}".format(digest.replace("sha256:", "sha256_"))
            for digest in sets.to_list(layer_digests)
        })
    files.update(referrer_blobs)

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

    # Construct purl for package_metadata
    identifier = digest if have_valid_digest else rctx.attr.tag
    purl = image_purl(rctx.attr.repository, identifier, registries)

    loads = [
        (str(Label("@package_metadata//rules:package_metadata.bzl")), "package_metadata"),
        (str(Label("@rules_img//img/private:import.bzl")), "image_import"),
        (str(Label("@rules_img//img:load.bzl")), "image_load"),
    ]
    maybe_lazy_layer_download = ""
    if rctx.attr.layer_handling == "lazy":
        # we need to load the download_layer rule if we are in lazy mode
        loads.append((str(Label("@rules_img//img/private:download_blobs.bzl")), "download_blobs"))
        maybe_lazy_layer_download = """
download_blobs(
    name = "layers",
    {auth_attr}
    digests = {layer_digests},
    sources = {sources},
    tags = ["requires-network"],
)
""".format(
            layer_digests = json.encode_indent(
                sets.to_list(layer_digests),
                prefix = "    ",
                indent = "    ",
            ).replace("sha256:", "sha256_"),
            sources = json.encode_indent(
                sources,
                prefix = "    ",
                indent = "    ",
            ),
            auth_attr = _lazy_download_auth_attr(rctx),
        )

    # Build target_compatible_with based on discovered platforms
    target_compatible_with = map_os_arch_to_constraints(sets.to_list(platforms_set))

    # write out the files
    rctx.file(
        "BUILD.bazel",
        content = """# This file was generated by the pull repository rule.
{loads}

package_metadata(
    name = "package_metadata",
    purl = {purl},
    visibility = ["//:__subpackages__"],
)
{maybe_lazy_layer_download}

image_import(
    name = "image_import",
    digest = {digest},
    data = {data},
    files = {files},
    omitted_manifests = {omitted_manifests},
    referrers = {referrers},
    local_referrers = {local_referrers},
    registries = {registries},
    repository = {repository},
    tag = {tag},
    visibility = ["//visibility:private"],
)

alias(
    name = "image",
    actual = ":image_import",
    target_compatible_with = {target_compatible_with},
    visibility = ["//visibility:public"],
)

alias(
    name = {name},
    actual = ":image",
    visibility = ["//visibility:public"],
)

alias(
    name = "final",
    actual = ":image_import",
    visibility = ["//visibility:public"],
)

image_load(
    name = "load",
    image = ":final",
    registry = {load_registry},
    repository = {repository},
    tag = {tag},
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
            omitted_manifests = json.encode_indent(
                omitted_manifests,
                prefix = "    ",
                indent = "    ",
            ),
            referrers = json.encode_indent(
                referrers,
                prefix = "    ",
                indent = "    ",
            ),
            local_referrers = json.encode_indent(
                local_referrers,
                prefix = "    ",
                indent = "    ",
            ),
            registries = json.encode_indent(
                registries,
                prefix = "    ",
                indent = "    ",
            ),
            purl = repr(purl),
            repository = repr(rctx.attr.repository),
            tag = repr(rctx.attr.tag) if rctx.attr.tag else "None",
            load_registry = repr(registries[0]),
        ),
    )
    rctx.file("REPO.bazel", """\
repo(
    default_package_metadata = ["//:package_metadata"],
)
""")
    if len(rctx.attr.digest) > 0 and hasattr(rctx, "repo_metadata"):
        # allows participating in repo contents cache, unless the referrers depend on
        # what the registry lists right now
        return rctx.repo_metadata(reproducible = len(rctx.attr.referrer_artifact_types) == 0)

    # only to make buildifier happy
    return None

_DISCOVERED_REFERRERS = "discovered_referrers.json"

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

By default, all child manifests of a multi-platform image index are downloaded. Use the
`platforms` attribute to restrict the pull to the platforms you build for.
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
        "platforms": attr.string_list(
            doc = """Platforms to download from a multi-platform image index.

Each entry is an `"os/architecture"` or `"os/architecture/variant"` string
(e.g. `["linux/amd64", "linux/arm64"]`). Platforms are normalized before they are compared,
so `"linux/arm64"` also matches an index entry declaring `arm64` with variant `v8`. An entry
that names no variant matches every variant of that OS/architecture, so adding this
attribute never changes which child manifest a build ends up using - it only drops the
platforms you did not list. Name a variant (`"linux/amd64/v3"`) to select just that one.

If omitted (the default), every child manifest of the index is downloaded. Since a registry
counts each child manifest as a separate pull, restricting the list to the platforms you
actually build for can cut a cold fetch of a typical multi-arch base image from tens of
requests to a handful. Attestation manifests (which buildkit publishes with the platform
`unknown/unknown`) are dropped unless `"unknown/unknown"` is listed explicitly.

Fetching fails if a requested platform has no matching manifest in the index. The attribute
has no effect when pulling a single-platform image (the digest refers to that manifest, so
there is nothing to skip).

**Note:** the index blob is stored verbatim, so it keeps listing every platform while only
the selected children are available locally. A filtered image is meant to be used as a base
image; pushing or loading its unmodified index is not supported.""",
        ),
        "referrer_digests": attr.string_list(
            doc = """Digests of trusted referrer manifests (SBOMs, signatures, attestations, ...) to pull with the image.

Each referrer is fetched by digest from the image's registries and repository, along with its
config and layers -- regardless of `layer_handling`, since artifacts like an SBOM are meant to be
read during the build. The empty config most artifacts use is never fetched.

A referrer is attributed to the manifest named by its own `subject` field: the pulled manifest or
index, or one of the child manifests of the index. Fetching fails if a referrer has no subject, or
if its subject is not part of the pulled image. Only image manifests are supported as referrers.

The referrers are exposed through the image's `PulledReferrersInfo` provider. Use, for example,
`oras discover` or `crane manifest` on the referrers API to find the digests to list.""",
        ),
        "referrer_artifact_types": attr.string_list(
            doc = """Artifact types of referrers to discover and pull, without knowing their digests.

For the pulled manifest or index and, for an index, every child manifest that is downloaded (see
`platforms`), the referrers of these artifact types are listed and pulled like those in
`referrer_digests`. Listing uses the registry's referrers API, falling back to the referrers tag
schema (`sha256-<hex>`) on registries that don't support it.

This is meant for artifacts whose digests can't be known in advance, such as every signature of an
image, which a policy can then verify downstream. For example:
`["application/vnd.cncf.notary.signature", "application/vnd.dev.sigstore.bundle.v0.3+json"]`.
`"*"` stands for any artifact type: `["*"]` pulls every referrer.

Listing follows the distribution spec: only a `404` from the referrers API falls back to the tag
schema, paginated responses are followed, and a referrers tag that is not an image index means
there are no referrers. Since a referrers tag is maintained by clients rather than the registry,
every manifest it lists is fetched, and kept only if its own artifact type matches and its
`subject` is the manifest. Discovery always uses the `img` tool, even with `downloader = "bazel"`,
because Bazel's downloader exposes neither response status nor headers.

Because the result depends on what the registry lists at fetch time, the repository is not
reproducible when this is set (it does not participate in the repository contents cache).""",
        ),
        "referrer_artifacts": attr.label_list(
            doc = """Referrer artifact spec files describing artifacts from other sources, such as an SBOM database.

Each spec is a JSON file, typically written by your own repository rule, that lists artifacts and
the files that make them up. At fetch time, every artifact becomes an OCI artifact manifest (an
empty config, the files as layers) whose `subject` is the pulled manifest, so it is exposed through
`PulledReferrersInfo` exactly like a referrer from `referrer_digests`. See
[the referrers documentation](/docs/referrers.md) for the format.""",
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
        "credential_helper": attr.string(
            doc = """Credential helper to use for registry authentication when this repository rule runs the pull tool.

If omitted, the pull tool inherits `$IMG_CREDENTIAL_HELPER` (or `$IMG_CREDENTIAL_HELPER_OCI_REGISTRY`, which takes precedence) when present.""",
        ),
        "config": attr.label(
            allow_single_file = True,
            doc = """Label of a Docker-compatible registry authentication config file (for example `//images:config.json`).

This is the label-based alternative to `docker_config_path` and mutually exclusive with it.
The file is used as `$REGISTRY_AUTH_FILE` for the pull tool. With lazy layer handling, it is also
declared as an input of the build actions that download layers.

**WARNING:** Because the file is an action input, it may be uploaded to a remote cache or remote
executor. Do not put secrets (`auth` or `identitytoken` entries) in it. Instead, reference a
credential helper, for example `{"credHelpers": {"ghcr.io": "gh"}}` or `{"credsStore": "desktop"}`.""",
        ),
        "docker_config_path": attr.string(
            doc = """Path to Docker-compatible registry authentication config.

If omitted, the pull tool inherits `$REGISTRY_AUTH_FILE` when present.""",
        ),
        "timeout": attr.int(
            default = 0,
            doc = """Maximum duration in seconds of each download performed while fetching this repository.

**Only takes effect when `downloader` is `img_tool`**, since it is applied to the `img` tool
invocations this rule runs. Bazel's own downloader cannot be given a timeout from a repository
rule, so this attribute is ignored when `downloader` is `bazel` (scale Bazel's built-in HTTP
timeouts with `--http_timeout_scaling` instead).

Defaults to `0`, which leaves the timeout unset, so Bazel's default for executions in
repository rules (600 seconds) applies.""",
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
