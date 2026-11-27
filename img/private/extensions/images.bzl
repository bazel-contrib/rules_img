"""Module extension for pulling container images."""

load("@bazel_skylib//lib:sets.bzl", "sets")
load("//img/private/extensions:images_helpers.bzl", "build_facts_to_store", "build_image_files_dict", "build_reverse_blob_mappings", "collect_blobs_to_create", "get_merged_sources_from_images", "get_registries_from_image", "merge_pull_attrs", "merge_sources", "normalize_repository_name", "pull_tag_to_struct", "sync_oci_ref_graph")
load("//img/private/extensions:images_referrers.bzl", "discovery_facts", "images_containing", "referrer_request", "referrers_for_image", "resolve_referrers")
load("//img/private/repository_rules:download.bzl", "learn_digest_from_tag", "resolve_docker_config_path")
load("//img/private/repository_rules:image_repo.bzl", "image_repo")
load("//img/private/repository_rules:pull_blob.bzl", "pull_blob_file", "pull_manifest_blob")

def _images_impl(ctx):
    """Implementation of the images module extension."""

    # Collect all image definitions from all modules.
    # We want to create one repository per image digest.
    images_by_digest = {}
    digest_visibility = {}
    root_module_images_by_name = {}

    # oci_ref_graph contains mapping from oci image manifets / indices to their referenced blobs
    # manifest: digest -> {"kind": "manifest", "config": "sha256:...", "layers": ["sha256:...", ...]}
    # index: digest -> {"kind": "index", "manifests": ["sha256:...", ...]}
    oci_ref_graph = {}

    # Access facts from previous extension evaluation for caching.
    # Facts are persisted in the lockfile and contain the OCI reference graph
    # to avoid re-downloading manifests on subsequent runs.
    facts = getattr(ctx, "facts", {})

    # Determine downloader to use for root module
    downloader = "img_tool"
    credential_helper = None
    docker_config_path = None
    config = None
    timeout = 0
    expose_hub_repo = "auto"
    expose_image_repos = "auto"
    for mod in ctx.modules:
        for settings in mod.tags.settings:
            if not mod.is_root:
                continue

            downloader = settings.downloader
            credential_helper = settings.credential_helper
            docker_config_path = settings.docker_config_path
            config = settings.config
            timeout = settings.timeout
            expose_hub_repo = settings.hub_repo
            expose_image_repos = settings.image_repos

    # Executions of the extension itself need a plain path. The repositories created
    # below receive the label instead, so lazy downloads can declare it as an input.
    extension_docker_config_path = resolve_docker_config_path(ctx, docker_config_path, config)

    # TODO(malt3): Add feature detection for dict in root_module_direct_deps
    support_dict_in_root_module_direct_deps = False
    expose_hub_repo = expose_hub_repo == "enabled" or (expose_hub_repo == "auto" and not support_dict_in_root_module_direct_deps)
    expose_image_repos = expose_image_repos == "enabled" or (expose_image_repos == "auto" and support_dict_in_root_module_direct_deps)

    resolved_any_tag = False
    referrer_requests = []
    for mod in ctx.modules:
        names = sets.make()
        digests = sets.make()
        for img in mod.tags.pull:
            digest = img.digest
            if not digest and img.unsafe_allow_tag_without_digest:
                if not img.tag:
                    fail("Image '{}' sets unsafe_allow_tag_without_digest but has no tag to resolve.".format(img.repository))
                digest = learn_digest_from_tag(
                    ctx,
                    tag = img.tag,
                    downloader = downloader,
                    sources = pull_tag_to_struct(img).sources,
                    credential_helper = credential_helper,
                    docker_config_path = extension_docker_config_path,
                    timeout = timeout,
                )
                resolved_any_tag = True
            sets.insert(digests, digest)
            name = img.name or img.repository
            if sets.contains(names, name):
                fail("Duplicate image name '{}' in module '{}@{}'. Please use unique names for images within the same module.".format(name, mod.name, mod.version))
            sets.insert(names, name)

            # check that digest is well-formed
            if not digest or not digest.startswith("sha256:") or len(digest) != 71:
                fail("Invalid digest '{}' for image '{}'. Must be of the form 'sha256:<64-hex-chars>'.".format(digest, img.repository))
            if digest not in images_by_digest:
                images_by_digest[digest] = pull_tag_to_struct(img, digest)
            else:
                images_by_digest[digest] = merge_pull_attrs(images_by_digest[digest], pull_tag_to_struct(img, digest), other_is_root = mod.is_root)
            if digest not in digest_visibility:
                digest_visibility[digest] = []
            visibility_identifier = "{}/{}/{}".format(mod.name, mod.version, img.name or img.repository)
            digest_visibility[digest].append(visibility_identifier)
            if mod.is_root:
                root_module_images_by_name[normalize_repository_name(img.name, img.repository)] = digest
            request = referrer_request(
                anchor = digest,
                tag = img,
                is_root = mod.is_root,
                where = "images.pull of '{}' in module '{}@{}'".format(name, mod.name, mod.version),
            )
            if request != None:
                referrer_requests.append(request)
        for tag in mod.tags.referrers:
            request = referrer_request(
                anchor = tag.manifest,
                tag = tag,
                is_root = mod.is_root,
                where = "images.referrers of {} in module '{}@{}'".format(tag.manifest, mod.name, mod.version),
            )
            if request != None:
                referrer_requests.append(request)

    # Sync OCI reference graph by downloading manifests (and referrer manifests)
    synced = sync_oci_ref_graph(
        ctx,
        images_by_digest,
        facts,
        downloader,
        credential_helper = credential_helper,
        docker_config_path = extension_docker_config_path,
        timeout = timeout,
        referrer_requests = referrer_requests,
    )
    oci_ref_graph = synced.graph

    # Attribute every referrer to the manifest named by its subject.
    resolved_referrers = resolve_referrers(referrer_requests, images_by_digest, oci_ref_graph, synced.discovered)

    # Build reverse mappings from blobs to top-level images for fast lookups
    file_blob_to_images, manifest_blob_to_images = build_reverse_blob_mappings(oci_ref_graph, images_by_digest)

    # Create blob repositories for all required blobs (deduplicated and content-addressable).
    # Referrer manifests are part of the graph too, but get blob repositories of their own below.
    image_ref_graph = {digest: entry for digest, entry in oci_ref_graph.items() if digest in manifest_blob_to_images}
    manifest_blobs, file_blobs, lazy_file_blobs = collect_blobs_to_create(image_ref_graph, images_by_digest)

    # Create blob repositories for manifest/index blobs (deduplicated)
    for digest in manifest_blobs.keys():
        # Use reverse mapping to find all source images for this manifest blob
        if digest not in manifest_blob_to_images or len(manifest_blob_to_images[digest]) == 0:
            fail("Could not find source image for manifest/index digest '{}'.".format(digest))

        # Build merged sources from all images that serve this blob
        sources = get_merged_sources_from_images(manifest_blob_to_images[digest], images_by_digest)

        repo_name = "blob_{}".format(digest.replace("sha256:", "").replace(":", "_"))
        pull_manifest_blob(
            name = repo_name,
            sources = sources,
            digest = digest,
            downloader = downloader,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
            config = config,
            timeout = timeout,
        )

    # Create blob repositories for config/layer blobs (deduplicated, eager)
    for digest in file_blobs.keys():
        # Use reverse mapping to find all source images for this file blob
        if digest not in file_blob_to_images or len(file_blob_to_images[digest]) == 0:
            fail("Could not find source image for config/layer blob digest '{}'.".format(digest))

        # Build merged sources from all images that serve this blob
        sources = get_merged_sources_from_images(file_blob_to_images[digest], images_by_digest)

        repo_name = "blob_{}".format(digest.replace("sha256:", "").replace(":", "_"))
        pull_blob_file(
            name = repo_name,
            sources = sources,
            digest = digest,
            downloaded_file_path = "blob",
            handling = "eager",
            downloader = downloader,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
            config = config,
            timeout = timeout,
        )

    # Create blob repositories for lazy layer blobs (deduplicated, lazy)
    for digest in lazy_file_blobs.keys():
        # Use reverse mapping to find all source images for this file blob
        if digest not in file_blob_to_images or len(file_blob_to_images[digest]) == 0:
            fail("Could not find source image for lazy layer blob digest '{}'.".format(digest))

        # Build merged sources from all images that serve this blob
        sources = get_merged_sources_from_images(file_blob_to_images[digest], images_by_digest)

        repo_name = "lazy_{}".format(digest.replace("sha256:", "").replace(":", "_"))
        pull_blob_file(
            name = repo_name,
            sources = sources,
            digest = digest,
            downloaded_file_path = "blob",
            handling = "lazy",
            downloader = downloader,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
            config = config,
            timeout = timeout,
        )

    # Create blob repositories for the referrer manifests and all of their blobs
    # (eager, whatever the layer handling of the image), from the sources of the
    # images they apply to. Blobs already created above are reused.
    referrer_manifest_sources = {}
    referrer_blob_sources = {}
    for subject, referrers in resolved_referrers.by_manifest.items():
        sources = get_merged_sources_from_images(images_containing(subject, images_by_digest, oci_ref_graph), images_by_digest)
        for referrer in referrers:
            entry = oci_ref_graph[referrer]
            targets = [(referrer_manifest_sources, referrer, manifest_blobs)]
            targets += [(referrer_blob_sources, blob, file_blobs) for blob in [entry.get("config")] + entry.get("layers", []) if blob]
            for (by_digest, blob_digest, existing) in targets:
                if blob_digest in existing:
                    continue
                merge_sources(by_digest.setdefault(blob_digest, {}), sources)
    for digest, sources in referrer_manifest_sources.items():
        pull_manifest_blob(
            name = "blob_{}".format(digest.replace("sha256:", "")),
            sources = sources,
            digest = digest,
            downloader = downloader,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
            config = config,
            timeout = timeout,
        )
    for digest, sources in referrer_blob_sources.items():
        pull_blob_file(
            name = "blob_{}".format(digest.replace("sha256:", "")),
            sources = sources,
            digest = digest,
            downloaded_file_path = "blob",
            handling = "eager",
            downloader = downloader,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
            config = config,
            timeout = timeout,
        )

    # Create image repositories for each top-level image
    for digest, img in images_by_digest.items():
        repo_name = "img_{}".format(digest.replace("sha256:", ""))

        # Build files dict with only referenced blobs for this image
        files = build_image_files_dict(digest, oci_ref_graph, img.layer_handling)
        image_referrers = referrers_for_image(digest, oci_ref_graph, resolved_referrers)
        for referrer in image_referrers.referrers:
            files[referrer] = "@blob_{}//:manifest.json".format(referrer.replace("sha256:", ""))
            entry = oci_ref_graph[referrer]
            for blob in [entry.get("config")] + entry.get("layers", []):
                if blob:
                    files[blob] = "@blob_{}//:blob".format(blob.replace("sha256:", ""))

        # Create the image repository
        image_repo(
            name = repo_name,
            digest = digest,
            files = files,
            registries = json.encode(get_registries_from_image(img)),
            repository = img.repository,
            tag = img.tag if hasattr(img, "tag") else None,
            referrers = image_referrers.referrers,
            referrer_artifacts = image_referrers.artifacts,
            referrer_subjects = {referrer: oci_ref_graph[referrer]["subject"] for referrer in image_referrers.referrers if oci_ref_graph[referrer].get("subject")},
        )

    # Create hub repository for convenient image access
    _create_hub_repo(
        name = "rules_img_images.bzl",
        digest_visibility = digest_visibility,
    )

    # ctx.root_module_has_non_dev_dependency
    root_module_direct_deps = {}
    if expose_hub_repo:
        root_module_direct_deps["rules_img_images.bzl"] = "rules_img_images.bzl"
    if expose_image_repos:
        for name, digest in root_module_images_by_name.items():
            repo_name = "img_{}".format(digest.replace("sha256:", ""))
            root_module_direct_deps[name] = repo_name

    # Flatten to list if mapping is not supported.
    if not support_dict_in_root_module_direct_deps:
        root_module_direct_deps = root_module_direct_deps.values()

    kwargs = {
        "root_module_direct_deps": root_module_direct_deps if ctx.root_module_has_non_dev_dependency else [],
        "root_module_direct_dev_deps": [] if ctx.root_module_has_non_dev_dependency else root_module_direct_deps,
        "reproducible": not resolved_any_tag,
    }
    if hasattr(ctx, "facts"):
        facts_to_store = build_facts_to_store(oci_ref_graph)
        facts_to_store.update(discovery_facts(synced.discovered))
        kwargs["facts"] = facts_to_store
    elif any([request.artifact_types for request in referrer_requests]):
        # Without facts, nothing records the discovered referrers between evaluations.
        kwargs["reproducible"] = False
    return ctx.extension_metadata(**kwargs)

def _create_hub_repo_impl(rctx):
    """Implementation of the hub repository rule."""
    images = {}
    for digest, visibility_list in rctx.attr.digest_visibility.items():
        repo_name = "img_{}".format(digest.replace("sha256:", ""))
        for visibility_id in visibility_list:
            # Extract friendly name from visibility identifier
            parts = visibility_id.split("/", 2)
            if len(parts) != 3:
                fail("Invalid visibility identifier '{}'.".format(visibility_id))
            module_name = parts[0]
            module_version = parts[1]
            friendly_name = parts[2]
            if module_name not in images:
                images[module_name] = {}
            if module_version not in images[module_name]:
                images[module_name][module_version] = {}
            images[module_name][module_version][friendly_name] = repo_name

    # Generate the helper macro file
    content = '''"""Helper macros for referencing pulled container images.

This file is auto-generated by the images module extension (@rules_img//img:extensions.bzl%images).
"""

_IMAGES = {}

def image(name):
    """Get the target for a pulled container image.

    Args:
        name: The friendly name of the image (e.g., "ubuntu:22.04", "distroless/cc")

    Returns:
        The label of the image target
    """
    module_name = native.module_name()
    module_version = native.module_version()
    if module_name not in _IMAGES:
        fail("Module '{{}}' has no images defined.".format(module_name))
    if module_version not in _IMAGES[module_name]:
        available_versions = ", ".join(sorted(_IMAGES[module_name].keys()))
        fail("Module '{{}}' has no images defined for version '{{}}'. Available versions: {{}}".format(module_name, module_version, available_versions))
    if name not in _IMAGES[module_name][module_version]:
        available_names = ", ".join(sorted(_IMAGES[module_name][module_version].keys()))
        fail("Image name '{{}}' not found in module '{{}}' version '{{}}'. Available names: {{}}".format(name, module_name, module_version, available_names))

    repo = _IMAGES[module_name][module_version][name]
    return Label("@{{}}//:image".format(repo))

'''.format(json.encode_indent(images, indent = "    "))

    rctx.file("rules_img_images.bzl", content)

    # Create a BUILD file
    rctx.file("BUILD.bazel", """
load("@bazel_skylib//:bzl_library.bzl", "bzl_library")

bzl_library(
    name = "rules_img_images",
    srcs = ["rules_img_images.bzl"],
    visibility = ["//visibility:public"],
)
""")

_create_hub_repo = repository_rule(
    implementation = _create_hub_repo_impl,
    attrs = {
        "digest_visibility": attr.string_list_dict(
            doc = "Maps from image digest to list of: module_name/module_version/friendly_name",
        ),
    },
)

_REFERRER_DIGESTS_DOC = """Digests of trusted referrer manifests (SBOMs, signatures, attestations, ...) to pull.

Each referrer is fetched by digest, along with its config and layers -- regardless of
`layer_handling`, since artifacts like an SBOM are meant to be read during the build. The empty
config most artifacts use is never fetched.

A referrer is attributed to the manifest named by its own `subject` field, which must be {anchor}
or, if that is an index, one of its child manifests. Evaluating the extension fails if a referrer
has no subject, or applies to any other manifest. Only image manifests are supported as referrers.
Cosign's tag-scheme signatures and attestations (see `referrer_cosign_tags`) have no subject: they
are attributed to the manifest that their signed payloads name.

The referrers are exposed through the `PulledReferrersInfo` provider of every pulled image containing the
manifest they apply to."""

_REFERRER_ARTIFACT_TYPES_DOC = """Artifact types of referrers to discover and pull, without knowing their digests.

For {anchor} and, if that is an index, every child manifest, the referrers of these artifact types
are listed and pulled like those in `referrer_digests`. Listing follows the distribution spec: the
registry's referrers API (following pagination), falling back to the referrers tag schema
(`sha256-<hex>`) only if the API answers `404`. Since a referrers tag is maintained by clients
rather than the registry, every manifest it lists is fetched, and kept only if its own artifact type
matches and its `subject` is the manifest. Discovery always uses the `img` tool, even with
`downloader = "bazel"`, because Bazel's downloader exposes neither response status nor headers.

This is meant for artifacts whose digests can't be known in advance, such as every signature of an
image, which a policy can then verify downstream. For example:
`["application/vnd.cncf.notary.signature", "application/vnd.dev.sigstore.bundle.v0.3+json"]`.
`"*"` stands for any artifact type: `["*"]` pulls every referrer, cosign's tag-scheme signatures
and attestations too (as `referrer_cosign_tags = ["sig", "att"]` does), and the BuildKit
attestations of the index (as `referrer_buildkit_attestations = True` does).

The discovered digests are recorded in the lockfile facts, per manifest and artifact type, so later
evaluations reuse them instead of querying the registry again. To pick up new referrers, drop the
`referrer_discovery@` facts of the manifest (or remove and re-add the artifact type). Without facts
support, the extension is not reproducible when this is set."""

_REFERRER_COSIGN_TAGS_DOC = """Cosign tag-scheme artifacts to discover and pull: `"sig"` for signatures, `"att"` for attestations.

Unless told to use OCI referrers, cosign doesn't attach signatures and attestations as referrers:
it pushes them as manifests tagged after the digest of the image, `sha256-<hex>.sig` and
`sha256-<hex>.att`, without a `subject`. For {anchor} and, if that is an index, every child
manifest, the manifests under these tags are pulled like referrers, and attributed to the manifest
they are tagged after. As the tag is only a naming convention, every layer of such a manifest is
checked to name that manifest in its signed payload: the `docker-manifest-digest` of a signature's
simple signing payload, or the subject of an attestation's in-toto statement. Any other content
fails the evaluation. Signatures are not verified.

The discovered digests are recorded in the lockfile facts like those of `referrer_artifact_types`
(as the artifact types `cosign:sig` and `cosign:att`)."""

_REFERRER_BUILDKIT_ATTESTATIONS_DOC = """Pull the BuildKit attestations (SBOMs, SLSA provenance) of the manifests of {anchor} as their referrers.

`docker buildx build --sbom=true --provenance=true` doesn't attach attestations as referrers: it
stores them in the image index, as an attestation manifest per platform manifest, with the
platform `unknown/unknown` and the annotations `vnd.docker.reference.type = attestation-manifest`
and `vnd.docker.reference.digest` naming the manifest it is about. Each of its layers is an in-toto
statement whose predicate type the layer's `in-toto.io/predicate-type` annotation names. With this
set, the attestation manifests that the index lists for its child manifests are pulled like
referrers of those manifests (whether or not the attestation manifest has a `subject`; BuildKit's
`oci-artifact=false` format has none), and the SBOMs among them are found by the SBOM toolchain of
an image built on one.

They are listed by the index, which its digest pins, so they are reproducible; they are recorded in
the lockfile facts like those of `referrer_artifact_types` (as the artifact type
`buildkit:attestation`). Attestation manifests that a registry lists as referrers (where the
BuildKit attestations were copied to referrers, or as Docker Hardened Images publish them) are
found with `referrer_artifact_types = ["application/vnd.docker.attestation.manifest.v1+json",
"application/vnd.in-toto+json"]` instead."""

_REFERRER_ARTIFACTS_DOC = """Referrer artifact spec files describing artifacts from other sources, such as an SBOM database.

Each spec is a JSON file, typically written by your own repository rule, that lists artifacts and
the files that make them up. When the image repository is fetched, every artifact becomes an OCI
artifact manifest (an empty config, the files as layers) whose `subject` is {anchor} (or the
`subject` the spec names, which must be {anchor} or one of its children), so it is exposed through
`PulledReferrersInfo` like a referrer from `referrer_digests`. See
[the referrers documentation](/docs/referrers.md) for the format."""

_pull_tag = tag_class(
    attrs = {
        "name": attr.string(
            doc = """Friendly name for the image (e.g., 'ubuntu', 'distroless-base').

This name is used to reference the image in your code via the `image()` helper function.
If not specified, defaults to the repository name.""",
        ),
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

While optional, it's recommended to also specify a digest for reproducible builds.""",
        ),
        "digest": attr.string(
            doc = """The image digest for reproducible pulls (e.g., "sha256:abc123...").

When specified, the image is pulled by digest instead of tag, ensuring reproducible builds.
The digest must be a full SHA256 digest starting with "sha256:".""",
        ),
        "referrer_digests": attr.string_list(
            doc = _REFERRER_DIGESTS_DOC.format(anchor = "the pulled manifest or index"),
        ),
        "referrer_artifact_types": attr.string_list(
            doc = _REFERRER_ARTIFACT_TYPES_DOC.format(anchor = "the pulled manifest or index"),
        ),
        "referrer_buildkit_attestations": attr.bool(
            doc = _REFERRER_BUILDKIT_ATTESTATIONS_DOC.format(anchor = "the pulled index"),
        ),
        "referrer_cosign_tags": attr.string_list(
            doc = _REFERRER_COSIGN_TAGS_DOC.format(anchor = "the pulled manifest or index"),
        ),
        "referrer_artifacts": attr.label_list(
            doc = _REFERRER_ARTIFACTS_DOC.format(anchor = "the pulled manifest or index"),
        ),
        "unsafe_allow_tag_without_digest": attr.bool(
            default = False,
            doc = """Resolve `tag` to a digest each time the extension is evaluated, instead of requiring `digest`.

**WARNING:** Builds then follow the tag, so they are not reproducible.

The extension reports itself as not reproducible, so Bazel re-evaluates it in each new
server instead of recording the digest in the lockfile. Image and blob repositories stay
keyed by digest, so they come from the repo contents cache until the tag moves. Their
repository names contain the digest: reference the image through the hub's `image()`.""",
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
  network access during the build phase. **EXPERIMENTAL:** Use at your own risk.""",
        ),
    },
)

_referrers_tag = tag_class(
    doc = """Attaches referrers to a manifest pulled by any module.

The manifest can be a pulled manifest or index, or a child manifest of a pulled index, and may be
pulled by another module than the one declaring the referrers:

```starlark
images.referrers(
    manifest = "sha256:...",
    referrer_digests = ["sha256:..."],
    referrer_artifact_types = ["application/vnd.cncf.notary.signature"],
)
```

In the root module, it is an error if no module pulls the manifest. In other modules, the tag is
ignored then, so a module can annotate images that only some of its users pull.""",
    attrs = {
        "manifest": attr.string(
            mandatory = True,
            doc = "Digest of the manifest or index the referrers apply to.",
        ),
        "referrer_digests": attr.string_list(
            doc = _REFERRER_DIGESTS_DOC.format(anchor = "`manifest`"),
        ),
        "referrer_artifact_types": attr.string_list(
            doc = _REFERRER_ARTIFACT_TYPES_DOC.format(anchor = "`manifest`"),
        ),
        "referrer_buildkit_attestations": attr.bool(
            doc = _REFERRER_BUILDKIT_ATTESTATIONS_DOC.format(anchor = "`manifest`, if it is an index,"),
        ),
        "referrer_cosign_tags": attr.string_list(
            doc = _REFERRER_COSIGN_TAGS_DOC.format(anchor = "`manifest`"),
        ),
        "referrer_artifacts": attr.label_list(
            doc = _REFERRER_ARTIFACTS_DOC.format(anchor = "`manifest`"),
        ),
    },
)

_settings_tag = tag_class(
    attrs = {
        "downloader": attr.string(
            default = "img_tool",
            values = ["img_tool", "bazel"],
            doc = """The tool to use for downloading manifests and blobs if the current module is the root module.

**Available options:**

* **`img_tool`** (default): Uses the `img` tool for all downloads.

* **`bazel`**: Uses Bazel's native HTTP capabilities for downloading manifests and blobs.
""",
        ),
        "hub_repo": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = """Controls visibility of the hub repository @rules_img_images.bzl for image access via the images macro.

**Available options:**

* **`auto`** (default): The hub repository is made visible if named repositories cannot be mapped in the current Bazel version.
                        This means you either get @rules_img_images.bzl or one repository per image.

* **`enabled`**: Always create and expose the hub repository @rules_img_images.bzl for image access via the images macro.

* **`disabled`**: Do not create the hub repository.
""",
        ),
        "credential_helper": attr.string(
            doc = """Credential helper to use for registry authentication when the module extension runs the pull tool.

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
credential helper, for example `{"credHelpers": {"ghcr.io": "gh"}}` or `{"credsStore": "desktop"}`.

Only honored in the root module.""",
        ),
        "docker_config_path": attr.string(
            doc = """Path to Docker-compatible registry authentication config.

If omitted, the pull tool inherits `$REGISTRY_AUTH_FILE` when present.""",
        ),
        "timeout": attr.int(
            default = 0,
            doc = """Maximum duration in seconds of each download performed for the pulled images.

**Only takes effect when `downloader` is `img_tool`**, since it is applied to the `img` tool
invocations run by this extension and by the repositories it creates. Bazel's own downloader
cannot be given a timeout from Starlark, so this setting is ignored when `downloader` is
`bazel` (scale Bazel's built-in HTTP timeouts with `--http_timeout_scaling` instead).

Defaults to `0`, which leaves the timeout unset, so Bazel's default for executions in module
extensions and repository rules (600 seconds) applies. Raise it when pulling large layers over
a slow connection.""",
        ),
        "image_repos": attr.string(
            default = "auto",
            values = ["auto", "enabled", "disabled"],
            doc = """Controls visibility of individual image repositories for direct access. Repos internally use the naming scheme img_<digest> and are mapped via friendly names (i.e. "ubuntu") if possible.

**Available options:**

* **`auto`** (default): Individual image repositories are made visible if named repositories can be mapped in the current Bazel version.
                        This means you either get one repository per image or @rules_img_images.bzl.

* **`enabled`**: Always expose individual image repositories for direct access.

* **`disabled`**: Do not expose individual image repositories.
""",
        ),
    },
)

images = module_extension(
    doc = """Module extension for pulling container images in Bzlmod projects.

This extension enables declarative pulling of container images using Bazel's module
system. Images are pulled once and shared across all modules, with automatic deduplication
of blobs for efficient storage.

Example usage in MODULE.bazel:

```starlark
images = use_extension("@rules_img//img:extensions.bzl", "images")

# Pull with friendly name
images.pull(
    name = "ubuntu",
    digest = "sha256:1e622c5f073b4f6bfad6632f2616c7f59ef256e96fe78bf6a595d1dc4376ac02",
    registry = "index.docker.io",
    repository = "library/ubuntu",
    tag = "24.04",
)

# Pull without name - use repository as identifier
images.pull(
    digest = "sha256:029d4461bd98f124e531380505ceea2072418fdf28752aa73b7b273ba3048903",
    registry = "gcr.io",
    repository = "distroless/base",
)

use_repo(images, "rules_img_images.bzl")
```

Access pulled images in BUILD files using the generated helper. The `name` attribute
is optional - if not specified, use the `repository` value to reference the image:

```starlark
load("@rules_img_images.bzl", "image")

image_manifest(
    name = "my_app",
    base = image("ubuntu"),  # References the friendly name
    ...
)

image_manifest(
    name = "my_other_app",
    base = image("distroless/base"),  # References the repository
    ...
)
```

The extension creates deduplicated blob repositories, so pulling multiple images
from the same base only downloads shared layers once. The `digest` parameter is
required for reproducibility.""",
    implementation = _images_impl,
    tag_classes = {
        "pull": _pull_tag,
        "referrers": _referrers_tag,
        "settings": _settings_tag,
    },
)
