"""Module extension for pulling container images."""

load("@bazel_skylib//lib:sets.bzl", "sets")
load("//img/private/extensions:images_helpers.bzl", "build_facts_to_store", "build_image_files_dict", "build_reverse_blob_mappings", "collect_blobs_to_create", "download_and_parse_manifest", "get_registries_from_image", "merge_pull_attrs", "pull_tag_to_struct")
load("//img/private/repository_rules:image_repo.bzl", "image_repo")
load("//img/private/repository_rules:pull_blob.bzl", "pull_blob_file", "pull_manifest_blob")

def _images_impl(ctx):
    """Implementation of the images module extension."""

    # Collect all image definitions from all modules.
    # We want to create one repository per image digest.
    images_by_digest = {}
    digest_visibility = {}

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
    for mod in ctx.modules:
        for settings in mod.tags.settings:
            if not mod.is_root:
                continue

            # Store downloader choice in facts for later use
            downloader = settings.downloader

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
                images_by_digest[digest] = merge_pull_attrs(images_by_digest[digest], img, other_is_root = mod.is_root)
            if digest not in digest_visibility:
                digest_visibility[digest] = []
            visibility_identifier = "{}/{}/{}".format(mod.name, mod.version, img.name or img.repository)
            digest_visibility[digest].append(visibility_identifier)

    # Download top-level manifests/indexes
    for digest, img in images_by_digest.items():
        ref_graph_entry, _manifest_data = download_and_parse_manifest(ctx, digest, img, facts, downloader)
        oci_ref_graph[digest] = ref_graph_entry

    # Download child manifests referenced by indexes
    manifest_to_download_from_index = {}
    for parent_digest, ref_graph_entry in oci_ref_graph.items():
        if ref_graph_entry["kind"] == "index":
            for child_digest in ref_graph_entry["manifests"]:
                if child_digest not in oci_ref_graph:
                    manifest_to_download_from_index[child_digest] = parent_digest

    for digest, index_digest in manifest_to_download_from_index.items():
        img = images_by_digest[index_digest]
        ref_graph_entry, _manifest_data = download_and_parse_manifest(ctx, digest, img, facts, downloader)
        if ref_graph_entry["kind"] != "manifest":
            fail("Expected manifest for digest '{}' but got '{}'.".format(digest, ref_graph_entry["kind"]))
        oci_ref_graph[digest] = ref_graph_entry

    # Build reverse mappings from blobs to top-level images for fast lookups
    file_blob_to_images, manifest_blob_to_images = build_reverse_blob_mappings(oci_ref_graph, images_by_digest)

    # Create blob repositories for all required blobs (deduplicated and content-addressable)
    manifest_blobs, file_blobs, lazy_file_blobs = collect_blobs_to_create(oci_ref_graph, images_by_digest)

    # Create blob repositories for manifest/index blobs (deduplicated)
    for digest in manifest_blobs.keys():
        # Use reverse mapping to find source image for this manifest blob
        if digest not in manifest_blob_to_images or len(manifest_blob_to_images[digest]) == 0:
            fail("Could not find source image for manifest/index digest '{}'.".format(digest))
        source_image_digest = manifest_blob_to_images[digest][0]
        source_img = images_by_digest[source_image_digest]

        # Get registry info from source image
        registry = None
        registries = get_registries_from_image(source_img)
        if hasattr(source_img, "registry") and source_img.registry:
            registry = source_img.registry

        repo_name = "blob_{}".format(digest.replace("sha256:", "").replace(":", "_"))
        pull_manifest_blob(
            name = repo_name,
            registry = registry,
            registries = registries,
            repository = source_img.repository,
            digest = digest,
            downloader = downloader,
        )

    # Create blob repositories for config/layer blobs (deduplicated, eager)
    for digest in file_blobs.keys():
        # Use reverse mapping to find source image for this file blob
        if digest not in file_blob_to_images or len(file_blob_to_images[digest]) == 0:
            fail("Could not find source image for config/layer blob digest '{}'.".format(digest))
        source_image_digest = file_blob_to_images[digest][0]
        source_img = images_by_digest[source_image_digest]

        # Get registry info from source image
        registries = get_registries_from_image(source_img)
        registry = None
        if hasattr(source_img, "registry") and source_img.registry:
            registry = source_img.registry

        repo_name = "blob_{}".format(digest.replace("sha256:", "").replace(":", "_"))
        pull_blob_file(
            name = repo_name,
            registry = registry,
            registries = registries,
            repository = source_img.repository,
            digest = digest,
            downloaded_file_path = "blob",
            handling = "eager",
            downloader = downloader,
        )

    # Create blob repositories for lazy layer blobs (deduplicated, lazy)
    for digest in lazy_file_blobs.keys():
        # Use reverse mapping to find source image for this file blob
        if digest not in file_blob_to_images or len(file_blob_to_images[digest]) == 0:
            fail("Could not find source image for lazy layer blob digest '{}'.".format(digest))
        source_image_digest = file_blob_to_images[digest][0]
        source_img = images_by_digest[source_image_digest]

        # Get registry info from source image
        registries = get_registries_from_image(source_img)
        registry = None
        if hasattr(source_img, "registry") and source_img.registry:
            registry = source_img.registry

        repo_name = "lazy_{}".format(digest.replace("sha256:", "").replace(":", "_"))
        pull_blob_file(
            name = repo_name,
            registry = registry,
            registries = registries,
            repository = source_img.repository,
            digest = digest,
            downloaded_file_path = "blob",
            handling = "lazy",
            downloader = downloader,
        )

    # Create image repositories for each top-level image
    for digest, img in images_by_digest.items():
        repo_name = "img_{}".format(digest.replace("sha256:", ""))

        # Build files dict with only referenced blobs for this image
        files = build_image_files_dict(digest, oci_ref_graph, img.layer_handling)

        # Create the image repository
        image_repo(
            name = repo_name,
            digest = digest,
            files = files,
            registries = json.encode(get_registries_from_image(img)),
            repository = img.repository,
            tag = img.tag if hasattr(img, "tag") else None,
        )

    # Create hub repository for convenient image access
    _create_hub_repo(
        name = "rules_img_images.bzl",
        digest_visibility = digest_visibility,
    )

    kwargs = {
        "root_module_direct_deps": ["rules_img_images.bzl"],
        "root_module_direct_dev_deps": [],
        "reproducible": True,
    }
    if hasattr(ctx, "facts"):
        kwargs["facts"] = build_facts_to_store(oci_ref_graph)
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
  This ensures layers are accessible in build actions but is inefficient as all layers
  are downloaded regardless of whether they're needed. Use this for base images that
  need to be read or inspected during the build.

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
        "settings": _settings_tag,
    },
)
