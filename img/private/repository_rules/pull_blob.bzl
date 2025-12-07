"""Repository rule for pulling individual blobs from a container registry."""

load("@pull_hub_repo//:defs.bzl", "tool_for_repository_os")
load("//img/private/repository_rules:download.bzl", "download_blob", "download_manifest_rctx")

def _pull_blob_file_impl(rctx):
    if rctx.attr.handling == "eager":
        # Eager: Download the blob in the repository rule
        result = download_blob(
            rctx,
            downloader = rctx.attr.downloader,
            digest = rctx.attr.digest,
            output = rctx.attr.downloaded_file_path,
            wait_and_read = False,
        )
        if result.waiter != None:
            result.waiter.wait()

        rctx.file(
            "BUILD.bazel",
            content = """exports_files([{}])

filegroup(
    name = "output",
    srcs = [{}],
    visibility = ["//visibility:public"],
)""".format(repr(rctx.attr.downloaded_file_path), repr(rctx.attr.downloaded_file_path)),
        )
        rctx.file(
            "file/BUILD.bazel",
            content = """alias(
    name = "file",
    actual = "//:output",
    visibility = ["//visibility:public"],
)""",
        )
    else:  # lazy
        # Lazy: Generate a BUILD file with download_blobs rule
        # Build registries list
        registries = []
        if rctx.attr.registries:
            registries = list(rctx.attr.registries)
        if rctx.attr.registry:
            registries.append(rctx.attr.registry)

        # Use sha256_ prefix for lazy blob output name
        digest_filename = rctx.attr.digest.replace("sha256:", "sha256_")

        rctx.file(
            "BUILD.bazel",
            content = """load("@rules_img//img/private:download_blobs.bzl", "download_blobs")

download_blobs(
    name = "blob",
    digests = ["{digest_filename}"],
    registries = {registries},
    repository = {repository},
    tags = ["requires-network"],
    visibility = ["//visibility:public"],
)
""".format(
                digest_filename = digest_filename,
                blob_name = repr(rctx.attr.downloaded_file_path),
                registries = json.encode_indent(
                    registries,
                    prefix = "    ",
                    indent = "    ",
                ),
                repository = repr(rctx.attr.repository),
            ),
        )
        rctx.file(
            "file/BUILD.bazel",
            content = """alias(
    name = "file",
    actual = "//:blob",
    visibility = ["//visibility:public"],
)""",
        )

    if len(rctx.attr.digest) > 0 and hasattr(rctx, "repo_metadata"):
        # allows participating in repo contents cache
        return rctx.repo_metadata(reproducible = True)

    # only to make buildifier happy
    return None

pull_blob_file = repository_rule(
    implementation = _pull_blob_file_impl,
    doc = """Pull a single blob from a container registry.""",
    attrs = {
        "registry": attr.string(
            doc = """Registry to pull from (e.g., "index.docker.io").""",
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
        "digest": attr.string(
            mandatory = True,
            doc = """The blob digest to pull (e.g., "sha256:abc123...").""",
        ),
        "downloaded_file_path": attr.string(
            default = "blob",
            doc = """Path assigned to the file downloaded.""",
        ),
        "executable": attr.bool(
            default = False,
            doc = """If the downloaded file should be made executable.""",
        ),
        "handling": attr.string(
            default = "eager",
            values = ["eager", "lazy"],
            doc = """Strategy for handling blob downloads.

**Available strategies:**

* **`eager`** (default): Blob data is fetched in the repository rule and is always available.

* **`lazy`**: Blob data is downloaded in a build action when requested. This avoids
  unnecessary downloads, but requires network access during the build phase.
  **EXPERIMENTAL:** Use at your own risk.""",
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
    },
)

def _pull_blob_archive_impl(rctx):
    tool = tool_for_repository_os(rctx)
    tool_path = rctx.path(tool)
    output_name = "archive.{}".format(rctx.attr.type if rctx.attr.type != "" else "tgz")
    result = download_blob(
        rctx,
        downloader = rctx.attr.downloader,
        digest = rctx.attr.digest,
        output = output_name,
        tool_path = tool_path,
        wait_and_read = False,
    )
    if result.waiter != None:
        result.waiter.wait()
    if output_name != result.path:
        rctx.symlink(
            result.path,
            output_name,
        )
    rctx.extract(
        archive = output_name,
        strip_prefix = rctx.attr.strip_prefix,
    )
    rctx.delete(output_name)
    rctx.delete("blobs")
    rctx.file(
        "BUILD.bazel",
        content = rctx.attr.build_file_content,
    )
    if len(rctx.attr.digest) > 0 and hasattr(rctx, "repo_metadata"):
        # allows participating in repo contents cache
        return rctx.repo_metadata(reproducible = True)

    # only to make buildifier happy
    return None

pull_blob_archive = repository_rule(
    implementation = _pull_blob_archive_impl,
    doc = """Pull and extract a blob from a container registry.""",
    attrs = {
        "registry": attr.string(
            mandatory = True,
            doc = """Registry to pull from (e.g., "index.docker.io").""",
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
        "digest": attr.string(
            mandatory = True,
            doc = """The blob digest to pull (e.g., "sha256:abc123...").""",
        ),
        "build_file_content": attr.string(
            mandatory = True,
            doc = """Content of the BUILD file to generate in the extracted directory.""",
        ),
        "type": attr.string(
            default = "",
            doc = """File extension for the downloaded archive (e.g., "tar.gz", "tgz", "tar").

If not specified, defaults to "tgz".""",
        ),
        "strip_prefix": attr.string(
            default = "",
            doc = """Prefix to strip from the extracted files.""",
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
    },
)

def _pull_manifest_blob_impl(rctx):
    have_valid_digest = True
    if len(rctx.attr.digest) != 71:
        have_valid_digest = False
    elif not rctx.attr.digest.startswith("sha256:"):
        have_valid_digest = False
    reference = rctx.attr.digest if have_valid_digest else rctx.attr.tag
    manifest_info = download_manifest_rctx(
        rctx,
        downloader = rctx.attr.downloader,
        reference = reference,
    )
    rctx.symlink(
        manifest_info.path,
        "manifest.json",
    )
    rctx.file(
        "digest",
        manifest_info.digest,
    )
    rctx.file(
        "BUILD.bazel",
        content = """exports_files(["manifest.json"])

filegroup(
    name = "manifest",
    srcs = ["manifest.json"],
    visibility = ["//visibility:public"],
)""",
    )
    if len(rctx.attr.digest) > 0 and hasattr(rctx, "repo_metadata"):
        # allows participating in repo contents cache
        return rctx.repo_metadata(reproducible = True)

    # only to make buildifier happy
    return None

pull_manifest_blob = repository_rule(
    implementation = _pull_manifest_blob_impl,
    doc = """Pull a manifest blob from a container registry.

This repository rule downloads a single manifest from a container registry, either by
digest or by tag. The manifest is made available as a filegroup target.

Example usage in MODULE.bazel:
```starlark
pull_manifest_blob = use_repo_rule("@rules_img//img:pull_blob.bzl", "pull_manifest_blob")

pull_manifest_blob(
    name = "ubuntu_manifest",
    digest = "sha256:1e622c5f073b4f6bfad6632f2616c7f59ef256e96fe78bf6a595d1dc4376ac02",
    registry = "index.docker.io",
    repository = "library/ubuntu",
)
```

The `digest` parameter is recommended for reproducible builds. If omitted, the `tag`
parameter must be specified instead.
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
        "digest": attr.string(
            doc = """The manifest digest for reproducible pulls (e.g., "sha256:abc123...").

When specified, the manifest is pulled by digest instead of tag, ensuring reproducible
builds. The digest must be a full SHA256 digest starting with "sha256:".""",
        ),
        "tag": attr.string(
            doc = """The image tag to pull (e.g., "latest", "24.04", "v1.2.3").

Only used if `digest` is not specified. It's recommended to use a digest for reproducible builds.""",
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
    },
)
