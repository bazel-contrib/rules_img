"""Repository rule for pulling individual blobs from a container registry."""

load("@pull_hub_repo//:defs.bzl", "tool_for_repository_os")
load("//img/private/repository_rules:download.bzl", "download_blob", "download_manifest")

def _pull_blob_file_impl(rctx):
    result = download_blob(
        rctx,
        downloader = rctx.attr.downloader,
        digest = rctx.attr.digest,
        wait_and_read = False,
    )
    if result.waiter != None:
        result.waiter.wait()
    rctx.symlink(
        result.path,
        rctx.attr.downloaded_file_path,
    )

    rctx.file(
        "BUILD.bazel",
        content = """filegroup(
    name = "output",
    srcs = [{}],
    visibility = ["//visibility:public"],
)""".format(repr(rctx.attr.downloaded_file_path)),
    )
    rctx.file(
        "file/BUILD.bazel",
        content = """alias(
    name = "file",
    actual = "//:output",
    visibility = ["//visibility:public"],
)""",
    )

pull_blob_file = repository_rule(
    implementation = _pull_blob_file_impl,
    doc = """Pull a single blob from a container registry.""",
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
        "downloaded_file_path": attr.string(
            default = "blob",
            doc = """Path assigned to the file downloaded.""",
        ),
        "executable": attr.bool(
            default = False,
            doc = """If the downloaded file should be made executable.""",
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
    manifest_info = download_manifest(
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
        content = """filegroup(
    name = "manifest",
    srcs = ["manifest.json"],
    visibility = ["//visibility:public"],
)""",
    )

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
