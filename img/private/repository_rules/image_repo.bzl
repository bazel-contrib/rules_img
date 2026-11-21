"""Repository rule for creating image repos from pulled blobs."""

load("@bazel_skylib//lib:sets.bzl", "sets")
load("//img/private/platforms:constraints.bzl", "map_os_arch_to_constraints")
load("//img/private/platforms:platforms.bzl", "has_constraint_setting")
load(":referrers.bzl", "build_referrer_artifacts", "referrer_blob_digests", "referrer_subject", "subject_descriptors")
load(":registry.bzl", "image_purl")

def _image_repo_impl(rctx):
    """Create an image repository from pulled blob repositories."""

    # Build the data dict by reading config and manifest from files
    data = {}
    platforms_set = sets.make()

    # Read the root manifest/index
    root_digest = rctx.attr.digest
    if root_digest not in rctx.attr.files:
        fail("Root digest {} not found in files.".format(root_digest))
    manifest_label = rctx.attr.files[root_digest]
    manifest_path = rctx.path(manifest_label)
    data[root_digest] = rctx.read(manifest_path)

    # Parse the root blob to determine if it's a manifest or index
    root_content = json.decode(data[root_digest])
    is_index = "manifests" in root_content

    if is_index:
        # This is an index, read all child manifests and their configs
        for child_manifest in root_content.get("manifests", []):
            child_digest = child_manifest.get("digest")
            if not child_digest:
                continue

            # Extract platform from index manifest entry
            platform = child_manifest.get("platform", {})
            if platform:
                os = platform.get("os", "")
                arch = platform.get("architecture", "")
                if os and arch and has_constraint_setting(os, arch):
                    sets.insert(platforms_set, "{}_{}".format(os, arch))

            if child_digest not in rctx.attr.files:
                fail("Child manifest digest {} not found in files.".format(child_digest))

            child_label = rctx.attr.files[child_digest]
            child_path = rctx.path(child_label)
            data[child_digest] = rctx.read(child_path)

            # Read the child manifest's config
            child_content = json.decode(data[child_digest])
            child_config_digest = child_content.get("config", {}).get("digest")
            if child_config_digest:
                if child_config_digest not in rctx.attr.files:
                    fail("Config digest {} not found in files.".format(child_config_digest))
                child_config_label = rctx.attr.files[child_config_digest]
                child_config_path = rctx.path(child_config_label)
                data[child_config_digest] = rctx.read(child_config_path)
    else:
        # This is a single-platform manifest, read its config
        config_digest = root_content.get("config", {}).get("digest")
        if config_digest:
            if config_digest not in rctx.attr.files:
                fail("Config digest {} not found in files.".format(config_digest))
            config_label = rctx.attr.files[config_digest]
            config_path = rctx.path(config_label)
            data[config_digest] = rctx.read(config_path)

            # Extract platform from config
            config = json.decode(data[config_digest])
            os = config.get("os", "")
            arch = config.get("architecture", "")
            if os and arch and has_constraint_setting(os, arch):
                sets.insert(platforms_set, "{}_{}".format(os, arch))

    # Build target_compatible_with based on discovered platforms
    target_compatible_with = map_os_arch_to_constraints(sets.to_list(platforms_set))

    # Referrers from registries: the extension attributed them already; checking
    # again here keeps the repository from trusting a stale lockfile.
    image_name = "{}@{}".format(rctx.attr.repository, root_digest)
    subjects = subject_descriptors(root_digest, data[root_digest])
    for referrer in rctx.attr.referrers:
        if referrer not in rctx.attr.files:
            fail("Referrer manifest {} not found in files.".format(referrer))
        data[referrer] = rctx.read(rctx.path(rctx.attr.files[referrer]))
        referrer_subject(referrer, json.decode(data[referrer]), subjects, image_name, rctx.attr.referrer_subjects)

    # Referrers from artifact specs, built locally into blobs/ and bound to the
    # anchor manifest (or a manifest of its tree that the spec names).
    local_referrers = []
    local_files = {}
    for anchor, labels in rctx.attr.referrer_artifacts.items():
        if anchor not in subjects:
            fail("Referrer artifacts apply to {}, which is not part of {}.".format(anchor, image_name))
        allowed = subjects if anchor == root_digest else {anchor: subjects[anchor]}
        for artifact in build_referrer_artifacts(rctx, specs = [Label(label) for label in labels], subjects = allowed, default_subject = anchor):
            if artifact.digest in data:
                continue
            content = rctx.read("blobs/" + artifact.digest.replace("sha256:", "sha256/"))
            data[artifact.digest] = content
            local_referrers.append(artifact.digest)
            for blob in [artifact.digest] + referrer_blob_digests(json.decode(content)):
                if blob not in rctx.attr.files:
                    local_files[blob] = "//:blobs/" + blob.replace("sha256:", "sha256/")

    # Construct purl for package_metadata
    identifier = rctx.attr.digest
    if not identifier:
        identifier = rctx.attr.tag
    purl = image_purl(rctx.attr.repository, identifier, json.decode(rctx.attr.registries))

    # Generate the BUILD file that creates the image_import
    # Convert files dict to a string representation for the BUILD file
    files_strs = []
    for digest, label in rctx.attr.files.items():
        files_strs.append("        {}: \"{}\",".format(repr(digest), str(label)))
    for digest, label in local_files.items():
        files_strs.append("        {}: \"{}\",".format(repr(digest), label))
    files_str = "{\n" + "\n".join(files_strs) + "\n}"

    rctx.file(
        "BUILD.bazel",
        content = """# This file was generated by the images module extension.
load("@package_metadata//rules:package_metadata.bzl", "package_metadata")
load("@rules_img//img/private:import.bzl", "image_import")

package_metadata(
    name = "package_metadata",
    purl = {purl},
    visibility = ["//:__subpackages__"],
)

image_import(
    name = "image",
    digest = {digest},
    data = {data},
    files = {files},
    referrers = {referrers},
    referrer_subjects = {referrer_subjects},
    local_referrers = {local_referrers},
    registries = {registries},
    repository = {repository},
    tag = {tag},
    target_compatible_with = {target_compatible_with},
    visibility = ["//visibility:public"],
)
""".format(
            digest = repr(rctx.attr.digest),
            data = json.encode(data),
            files = files_str,
            purl = repr(purl),
            registries = rctx.attr.registries,
            repository = repr(rctx.attr.repository),
            tag = repr(rctx.attr.tag) if rctx.attr.tag else "None",
            target_compatible_with = target_compatible_with,
            referrers = json.encode(rctx.attr.referrers),
            referrer_subjects = json.encode(rctx.attr.referrer_subjects),
            local_referrers = json.encode(local_referrers),
        ),
    )
    rctx.file("REPO.bazel", """\
repo(
    default_package_metadata = ["//:package_metadata"],
)
""")

image_repo = repository_rule(
    implementation = _image_repo_impl,
    doc = """Creates an image repository from pulled blobs.

This repository rule is used by the images module extension to create
image repositories that reference blob repositories for manifests, configs,
and layers.""",
    attrs = {
        "digest": attr.string(
            mandatory = True,
            doc = "The root digest (manifest or index) of the image.",
        ),
        "files": attr.string_keyed_label_dict(
            mandatory = True,
            doc = "Dict of digest -> label for blob files.",
        ),
        "registries": attr.string(
            mandatory = True,
            doc = "JSON-encoded list of registries.",
        ),
        "repository": attr.string(
            mandatory = True,
            doc = "The image repository name.",
        ),
        "tag": attr.string(
            doc = "The image tag (optional).",
        ),
        "referrers": attr.string_list(
            doc = "Digests of the referrer manifests (fetched from registries) of the image. Their manifests and blobs are in `files`.",
        ),
        "referrer_artifacts": attr.string_list_dict(
            doc = "Maps the digest of a manifest in the image's tree to the labels of referrer artifact specs that apply to it.",
        ),
        "referrer_subjects": attr.string_dict(
            doc = "The subjects the extension attributed the referrers to. Only used for referrers without a `subject` field: cosign's tag-scheme artifacts, attributed to the manifest their tag or signed payloads name.",
        ),
    },
)
