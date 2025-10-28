"""Module extension for pulling container images."""

load("@bazel_skylib//lib:sets.bzl", "sets")
load("//img/private/repository_rules:pull.bzl", "pull")

def _pull_tag_to_struct(tag):
    """Convert a pull tag to a struct for easier attribute access."""
    vals = {
        "repository": tag.repository,
        "digest": tag.digest,
        "layer_handling": tag.layer_handling,
    }
    if tag.registry:
        vals["registry"] = tag.registry
    if tag.registries:
        vals["registries"] = tag.registries
    if tag.tag:
        vals["tag"] = tag.tag
    return struct(**vals)

def _merge_pull_attrs(target, other, other_is_root):
    """Merge pull attributes into a single struct for repository rule."""
    attrs = {
        "digest": target.digest or other.digest,
    }
    if other_is_root and other.tag:
        attrs["tag"] = other.tag
    elif target.tag:
        attrs["tag"] = target.tag
    elif other.tag:
        attrs["tag"] = other.tag

    if target.repository == other.repository:
        # if the repositories match, we can merge registries
        registries = sets.make()
        if hasattr(target, "registry") and target.registry:
            sets.insert(registries, target.registry)
        if hasattr(other, "registry") and other.registry:
            sets.insert(registries, other.registry)
        if hasattr(target, "registries"):
            for reg in target.registries:
                sets.insert(registries, reg)
        if hasattr(other, "registries"):
            for reg in other.registries:
                sets.insert(registries, reg)
        registries = sorted(sets.to_list(registries))
        if len(registries) == 1:
            attrs["registry"] = registries[0]
        elif len(registries) > 1:
            attrs["registries"] = registries
        attrs["repository"] = target.repository
    else:
        # otherwise, we cannot merge registries
        if other_is_root:
            if hasattr(other, "registry") and other.registry:
                attrs["registry"] = other.registry
            if hasattr(other, "registries"):
                attrs["registries"] = other.registries
        elif hasattr(target, "registry") and target.registry:
            attrs["registry"] = target.registry
        elif hasattr(target, "registries"):
            attrs["registries"] = target.registries
        attrs["repository"] = other.repository if other_is_root else target.repository

    # apply the strictest layer handling available:
    # eager > lazy > shallow
    layer_handling_priority = {
        "eager": 3,
        "lazy": 2,
        "shallow": 1,
    }
    target_handling = getattr(target, "layer_handling", "shallow")
    other_handling = getattr(other, "layer_handling", "shallow")
    if layer_handling_priority[target_handling] >= layer_handling_priority[other_handling]:
        attrs["layer_handling"] = target_handling
    else:
        attrs["layer_handling"] = other_handling

    return struct(**attrs)

def _images_impl(ctx):
    """Implementation of the images module extension."""

    # Collect all image definitions from all modules.
    # We want to create one repository per image digest.
    images_by_digest = {}
    digest_visibility = {}

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
                images_by_digest[digest] = _pull_tag_to_struct(img)
            else:
                images_by_digest[digest] = _merge_pull_attrs(images_by_digest[digest], img, other_is_root = mod.is_root)
            if digest not in digest_visibility:
                digest_visibility[digest] = []
            visibility_identifier = "{}/{}/{}".format(mod.name, mod.version, img.name or img.repository)
            digest_visibility[digest].append(visibility_identifier)

    print("Found {} unique images to pull.".format(len(images_by_digest)))
    print("Images by digest: {}".format(images_by_digest))
    print("Digest visibility: {}".format(digest_visibility))

    for digest, img in images_by_digest.items():
        print("Creating pull repository for image '{}' with digest '{}'.".format(img.repository, digest))
        attributes = {k: getattr(img, k) for k in dir(img)}
        attributes["name"] = "img_{}".format(digest.replace("sha256:", ""))
        pull(**attributes)

    _create_hub_repo(
        name = "rules_img_images.bzl",
        digest_visibility = digest_visibility,
    )

    return ctx.extension_metadata(
        root_module_direct_deps = ["rules_img_images.bzl"],
        root_module_direct_dev_deps = [],
        reproducible = True,
    )

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
    print("module_name: ", native.module_name())
    print("module_version: ", native.module_version())
    print("package_name: ", native.package_name())
    print("repo_name: ", native.repo_name())
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

# Define the tag class for the extension
_pull_tag = tag_class(
    attrs = {
        "name": attr.string(
            doc = "Friendly name for the image (e.g., 'ubuntu:22.04').",
        ),
        "registry": attr.string(
            doc = """Primary registry to pull from (e.g., "index.docker.io", "gcr.io").""",
        ),
        "registries": attr.string_list(
            doc = """List of mirror registries to try in order.""",
        ),
        "repository": attr.string(
            mandatory = True,
            doc = """The image repository (e.g., "library/ubuntu", "distroless/cc").""",
        ),
        "tag": attr.string(
            doc = """The image tag (e.g., "latest", "24.04").""",
        ),
        "digest": attr.string(
            doc = """The image digest for reproducible pulls (e.g., "sha256:abc123...").""",
        ),
        "layer_handling": attr.string(
            default = "shallow",
            values = ["shallow", "eager", "lazy"],
            doc = """Strategy for handling image layers.""",
        ),
    },
)

# Define the module extension
images = module_extension(
    implementation = _images_impl,
    tag_classes = {
        "pull": _pull_tag,
    },
)
