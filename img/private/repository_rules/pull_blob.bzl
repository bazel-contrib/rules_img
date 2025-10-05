"""Repository rule for pulling individual blobs from a container registry."""

load("@pull_hub_repo//:defs.bzl", "tool_for_repository_os")

def _pull_blob_file_impl(rctx):
    tool = tool_for_repository_os(rctx)
    tool_path = rctx.path(tool)
    args = [
        tool_path,
        "download-blob",
        "--digest",
        rctx.attr.digest,
        "--repository",
        rctx.attr.repository,
        "--registry",
        rctx.attr.registry,
        "--output",
        rctx.attr.downloaded_file_path,
    ]
    if rctx.attr.executable:
        args.append("--executable")
    rctx.execute(args)
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
    },
)

def _pull_blob_archive_impl(rctx):
    tool = tool_for_repository_os(rctx)
    tool_path = rctx.path(tool)
    output_name = "archive.{}".format(rctx.attr.type if rctx.attr.type != "" else "tgz")
    args = [
        tool_path,
        "download-blob",
        "--digest",
        rctx.attr.digest,
        "--repository",
        rctx.attr.repository,
        "--registry",
        rctx.attr.registry,
        "--output",
        output_name,
    ]
    rctx.execute(args)
    rctx.extract(
        archive = output_name,
        strip_prefix = rctx.attr.strip_prefix,
    )
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
        ),
        "strip_prefix": attr.string(
            default = "",
            doc = """Prefix to strip from the extracted files.""",
        ),
    },
)
