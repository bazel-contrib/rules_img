"""Module extension for pulling container images."""

load("@bazel_skylib//lib:sets.bzl", "sets")
load("//img/private/extensions:images_helpers.bzl", "PLATFORM_FACT_PREFIX", "build_facts_to_store", "build_image_files_dict", "build_reverse_blob_mappings", "check_is_manifest", "collect_blobs_to_create", "discover_platforms", "get_merged_sources_from_images", "get_registries_from_image", "merge_pull_attrs", "normalize_repository_name", "pull_tag_to_struct", "sync_oci_ref_graph")
load("//img/private/repository_rules:image_repo.bzl", "image_repo")
load("//img/private/repository_rules:image_router.bzl", "image_router")
load("//img/private/repository_rules:pull_blob.bzl", "pull_blob_file", "pull_manifest_blob")
load("//img/private/repository_rules:repo_names.bzl", "blob_repo_name", "child_repo_name", "image_label", "image_repo_name", "lazy_blob_repo_name", "original_repo_name")

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
    expose_hub_repo = "auto"
    expose_image_repos = "auto"
    for mod in ctx.modules:
        for settings in mod.tags.settings:
            if not mod.is_root:
                continue

            downloader = settings.downloader
            credential_helper = settings.credential_helper
            docker_config_path = settings.docker_config_path
            expose_hub_repo = settings.hub_repo
            expose_image_repos = settings.image_repos

    # TODO(malt3): Add feature detection for dict in root_module_direct_deps
    support_dict_in_root_module_direct_deps = False
    expose_hub_repo = expose_hub_repo == "enabled" or (expose_hub_repo == "auto" and not support_dict_in_root_module_direct_deps)
    expose_image_repos = expose_image_repos == "enabled" or (expose_image_repos == "auto" and support_dict_in_root_module_direct_deps)

    for mod in ctx.modules:
        names = sets.make()
        digests = sets.make()
        for img in mod.tags.pull:
            digest = img.digest
            sets.insert(digests, digest)
            name = img.name or img.repository
            if sets.contains(names, name):
                fail("Duplicate image name '{}' in module '{}@{}'. Please use unique names for images within the same module.".format(name, mod.name, mod.version))
            sets.insert(names, name)

            # check that digest is well-formed
            if not digest or not digest.startswith("sha256:") or len(digest) != 71:
                fail("Invalid digest '{}' for image '{}'. Must be of the form 'sha256:<64-hex-chars>'.".format(digest, img.repository))
            if digest not in images_by_digest:
                images_by_digest[digest] = pull_tag_to_struct(img)
            else:
                images_by_digest[digest] = merge_pull_attrs(images_by_digest[digest], pull_tag_to_struct(img), other_is_root = mod.is_root)
            if digest not in digest_visibility:
                digest_visibility[digest] = []
            visibility_identifier = "{}/{}/{}".format(mod.name, mod.version, img.name or img.repository)
            digest_visibility[digest].append(visibility_identifier)
            if mod.is_root:
                root_module_images_by_name[normalize_repository_name(img.name, img.repository)] = digest

    # Sync OCI reference graph by downloading manifests
    oci_ref_graph = sync_oci_ref_graph(
        ctx,
        images_by_digest,
        facts,
        downloader,
        credential_helper = credential_helper,
        docker_config_path = docker_config_path,
    )

    # Build reverse mappings from blobs to top-level images for fast lookups
    file_blob_to_images, manifest_blob_to_images = build_reverse_blob_mappings(oci_ref_graph, images_by_digest)

    platform_facts = discover_platforms(
        ctx,
        oci_ref_graph,
        images_by_digest,
        manifest_blob_to_images,
        facts,
        downloader,
        credential_helper = credential_helper,
        docker_config_path = docker_config_path,
    )

    # Create blob repositories for all required blobs (deduplicated and content-addressable)
    manifest_blobs, file_blobs, lazy_file_blobs = collect_blobs_to_create(oci_ref_graph, images_by_digest)

    # Create blob repositories for manifest/index blobs (deduplicated)
    for digest in manifest_blobs.keys():
        # Use reverse mapping to find all source images for this manifest blob
        if digest not in manifest_blob_to_images or len(manifest_blob_to_images[digest]) == 0:
            fail("Could not find source image for manifest/index digest '{}'.".format(digest))

        # Build merged sources from all images that serve this blob
        sources = get_merged_sources_from_images(manifest_blob_to_images[digest], images_by_digest)

        repo_name = blob_repo_name(digest)
        pull_manifest_blob(
            name = repo_name,
            sources = sources,
            digest = digest,
            downloader = downloader,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
        )

    # Create blob repositories for config/layer blobs (deduplicated, eager)
    for digest in file_blobs.keys():
        # Use reverse mapping to find all source images for this file blob
        if digest not in file_blob_to_images or len(file_blob_to_images[digest]) == 0:
            fail("Could not find source image for config/layer blob digest '{}'.".format(digest))

        # Build merged sources from all images that serve this blob
        sources = get_merged_sources_from_images(file_blob_to_images[digest], images_by_digest)

        repo_name = blob_repo_name(digest)
        pull_blob_file(
            name = repo_name,
            sources = sources,
            digest = digest,
            downloaded_file_path = "blob",
            handling = "eager",
            downloader = downloader,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
        )

    # Create blob repositories for lazy layer blobs (deduplicated, lazy)
    for digest in lazy_file_blobs.keys():
        # Use reverse mapping to find all source images for this file blob
        if digest not in file_blob_to_images or len(file_blob_to_images[digest]) == 0:
            fail("Could not find source image for lazy layer blob digest '{}'.".format(digest))

        # Build merged sources from all images that serve this blob
        sources = get_merged_sources_from_images(file_blob_to_images[digest], images_by_digest)

        repo_name = lazy_blob_repo_name(digest)
        pull_blob_file(
            name = repo_name,
            sources = sources,
            digest = digest,
            downloaded_file_path = "blob",
            handling = "lazy",
            downloader = downloader,
            credential_helper = credential_helper,
            docker_config_path = docker_config_path,
        )

    # Create image repositories for each top-level image
    for digest, img in images_by_digest.items():
        common = dict(
            registries = json.encode(get_registries_from_image(img)),
            repository = img.repository,
            tag = img.tag if hasattr(img, "tag") else None,
        )
        entry = oci_ref_graph[digest]

        if entry["kind"] != "index":
            # A single-platform image needs no routing repository: there is one manifest, and
            # its own repository knows the platform it is for.
            image_repo(
                name = image_repo_name(digest),
                digest = digest,
                files = build_image_files_dict(digest, oci_ref_graph, img.layer_handling),
                select_platform = True,
                **common
            )
            continue

        # The index with every manifest, config and layer it refers to. Only reachable through
        # `:original`, so a build that just needs a base image never fetches all of it.
        original_repo = original_repo_name(digest)
        image_repo(
            name = original_repo,
            digest = digest,
            files = build_image_files_dict(digest, oci_ref_graph, img.layer_handling),
            **common
        )

        # One repository per entry of the index, keyed by the position of the entry rather than
        # by the digest it points at: an index may list one digest twice with different
        # descriptors, and each child keeps the descriptor of the entry referring to it.
        children = []
        platforms = []
        for position, descriptor in enumerate(entry.get("descriptors") or []):
            child_digest = descriptor["digest"]
            child_entry = oci_ref_graph.get(child_digest)
            if child_entry == None:
                fail("Child manifest '{}' of index '{}' was never discovered.".format(child_digest, digest))
            check_is_manifest(child_digest, child_entry)
            child_repo = child_repo_name(digest, position)
            image_repo(
                name = child_repo,
                digest = child_digest,
                descriptor = json.encode(descriptor),
                files = build_image_files_dict(child_digest, oci_ref_graph, img.layer_handling),
                **common
            )
            children.append(image_label(child_repo))

            # An index descriptor does not have to declare a platform. For the ones that do not,
            # discovery read it from the manifest's config, the same fallback `image_import`
            # applies when it imports a whole index.
            platforms.append(descriptor.get("platform") or platform_facts.get(PLATFORM_FACT_PREFIX + child_digest))

        image_router(
            name = image_repo_name(digest),
            digest = digest,
            repository = img.repository,
            registries = common["registries"],
            platforms = json.encode(platforms),
            children = children,
            original = image_label(original_repo),
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
            root_module_direct_deps[name] = image_repo_name(digest)

    # Flatten to list if mapping is not supported.
    if not support_dict_in_root_module_direct_deps:
        root_module_direct_deps = root_module_direct_deps.values()

    kwargs = {
        "root_module_direct_deps": root_module_direct_deps if ctx.root_module_has_non_dev_dependency else [],
        "root_module_direct_dev_deps": [] if ctx.root_module_has_non_dev_dependency else root_module_direct_deps,
        "reproducible": True,
    }
    if hasattr(ctx, "facts"):
        facts_to_store = build_facts_to_store(oci_ref_graph)
        facts_to_store.update(platform_facts)
        kwargs["facts"] = facts_to_store
    return ctx.extension_metadata(**kwargs)

def _create_hub_repo_impl(rctx):
    """Implementation of the hub repository rule."""
    images = {}
    for digest, visibility_list in rctx.attr.digest_visibility.items():
        repo_name = image_repo_name(digest)
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

def _image_repo(name):
    """Get the repository of a pulled container image.

    Args:
        name: The friendly name of the image (e.g., "ubuntu:22.04", "distroless/cc")

    Returns:
        The repository name
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

    return _IMAGES[module_name][module_version][name]

def image(name):
    """Get the manifest of a pulled container image for the target platform.

    Only the manifest, config and layers of the platform the build is for are downloaded. For a
    target platform the image has no manifest for, the target is incompatible, so wildcard builds
    skip whatever depends on it instead of failing.

    Args:
        name: The friendly name of the image (e.g., "ubuntu:22.04", "distroless/cc")

    Returns:
        The label of the image target
    """
    return Label("@{{}}//:image".format(_image_repo(name)))

def original_image(name):
    """Get a pulled container image exactly as it was pulled.

    The image index of a multi-platform image, or the manifest of a single-platform one, with the
    digest it was pulled with. Use it to push or load the image unaltered. Unlike `image`, it is
    compatible with every target platform, and requires the blobs of every platform.

    Args:
        name: The friendly name of the image (e.g., "ubuntu:22.04", "distroless/cc")

    Returns:
        The label of the image target
    """
    return Label("@{{}}//:original".format(_image_repo(name)))

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
  Layers are accessible in build actions, for the manifest the target platform selects;
  the layers of other platforms are not downloaded. Building the original index does
  download every platform's layers. Use this for base images that need to be read or
  inspected during the build.

* **`lazy`**: Layer data is downloaded in a build action when requested. This provides
  access to layers during builds while avoiding unnecessary downloads, but requires
  network access during the build phase. **EXPERIMENTAL:** Use at your own risk.""",
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
        "docker_config_path": attr.string(
            doc = """Path to Docker-compatible registry authentication config.

If omitted, the pull tool inherits `$REGISTRY_AUTH_FILE` when present.""",
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
load("@rules_img_images.bzl", "image", "original_image")

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

# Mirror the pulled image unaltered, with every platform it has.
image_push(
    name = "mirror_ubuntu",
    image = original_image("ubuntu"),
    repository = "my-org/ubuntu",
)
```

Every pulled image offers two targets:

* `image(...)` (`@repo//:image`) is the single manifest for the target platform, chosen with a
  `select()`. A build downloads the manifest, config and layers of the platform it builds for,
  and of no other. Selection honors `--platforms`, `image_manifest(platform = ...)` and
  `image_index(platforms = ...)`. For a target platform the image has no manifest for, the target
  is incompatible, so wildcard builds skip whatever depends on it instead of failing.
* `original_image(...)` (`@repo//:original`) is the image exactly as it was pulled - an index with
  all of its platforms, or a single manifest - keeping its digest and its attestations. It has no
  platform constraint, so it can be pushed or loaded from any host, and it requires the blobs of
  every platform.

Callers that used `image(...)` to push or load a whole index need `original_image(...)` instead.

The reference graph of the pulled images and the platforms they offer are recorded in
`MODULE.bazel.lock` as facts, on Bazel versions that support them. Discovering them fetches
manifests, and the configs of index entries that declare no platform, but never layers. Once they
are recorded, evaluating the extension fetches nothing at all and does not need the pull tool.
Without facts support, discovery repeats whenever the extension is reevaluated.

The extension creates deduplicated blob repositories, so pulling multiple images
from the same base only downloads shared layers once. The `digest` parameter is
required for reproducibility.""",
    implementation = _images_impl,
    tag_classes = {
        "pull": _pull_tag,
        "settings": _settings_tag,
    },
)
