"""Repository rules for downloading container image components."""

load("@pull_hub_repo//:defs.bzl", "tool_for_repository_os")
load(":registry.bzl", "get_registries")

def learn_digest_from_tag(rctx, *, tag, downloader):
    """Learn the digest of an image from its tag by downloading manifest headers.

    Args:
        rctx: Repository context.
        tag: The tag to resolve.
        downloader: "img_tool" or "bazel".

    Returns:
        The resolved digest as a string (e.g., "sha256:abc123...") or None if resolution failed.
    """
    registries = get_registries(rctx)

    if downloader == "bazel":
        # Use Bazel's download to get the manifest and extract its digest
        result = rctx.download(
            url = [
                "https://{registry}/v2/{repository}/manifests/{tag}".format(
                    registry = registry,
                    repository = rctx.attr.repository,
                    tag = tag,
                )
                for registry in registries
            ],
            output = "temp_manifest_for_digest_learning.json",
        )

        # The digest is the SHA256 of the downloaded manifest
        return "sha256:" + result.sha256
    else:
        # Use img_tool download-manifest command with --print-digest flag
        tool = tool_for_repository_os(rctx)
        tool_path = rctx.path(tool)
        args = [
            tool_path,
            "download-manifest",
            "--repository",
            rctx.attr.repository,
            "--tag",
            tag,
            "--print-digest",
        ] + [
            "--registry={}".format(registry)
            for registry in registries
        ]
        result = rctx.execute(args)
        if result.return_code != 0:
            # Failed to get digest
            return None

        # The digest is printed to stdout
        digest = result.stdout.strip()
        if len(digest) > 0 and digest.startswith("sha256:"):
            return digest

        return None

def _check_existing_blob(rctx, digest, wait_and_read = True):
    """Check if a blob with the given digest already exists.

    Args:
        rctx: Repository context.
        digest: The blob digest to check.
        wait_and_read: If True, read the data from disk if the blob exists.

    Returns:
        A struct containing digest, path, and data of the downloaded blob or None if it does not exist.
    """
    if len(digest) < 64:
        # invalid digest
        return None
    blob_path = "blobs/sha256/" + digest.removeprefix("sha256:")
    if not rctx.path(blob_path).exists:
        return None
    return struct(
        digest = digest,
        path = blob_path,
        data = rctx.read(blob_path) if wait_and_read else None,
        waiter = None,
    )

def download_blob(rctx, *, downloader, digest, wait_and_read = True, repository = None, registries = None, output = None, **kwargs):
    """Download a blob from a container registry using the specified downloader.

    Args:
        rctx: Repository context or module context.
        downloader: "img_tool" or "bazel".
        digest: The blob digest to download.
        wait_and_read: If True, wait for the download to complete and read the data.
                       If False, return a waiter that can be used to wait for the download.
        repository: The image repository (optional, extracted from rctx.attr if not provided).
        registries: List of registries (optional, extracted from rctx.attr if not provided).
        output: Optional output path for the downloaded blob. If not specified, defaults to "blobs/sha256/<sha256>".
        **kwargs: Additional arguments.

    Returns:
        A struct containing digest, path, and data of the downloaded blob.
    """
    sha256 = digest.removeprefix("sha256:")
    if output == None:
        output = "blobs/sha256/" + sha256
    if registries == None:
        registries = get_registries(rctx)
    if repository == None:
        repository = rctx.attr.repository

    # Only check for existing blob in default location if using default output path
    if output == "blobs/sha256/" + sha256:
        maybe_existing = _check_existing_blob(rctx, digest, wait_and_read)
        if maybe_existing != None:
            return maybe_existing
    if downloader == "bazel":
        result = rctx.download(
            url = [
                "{protocol}://{registry}/v2/{repository}/blobs/{digest}".format(
                    protocol = "https",
                    registry = registry,
                    repository = repository,
                    digest = digest,
                )
                for registry in registries
            ],
            sha256 = sha256,
            output = output,
            block = wait_and_read,
            **kwargs
        )
    elif downloader == "img_tool":
        tool = tool_for_repository_os(rctx)
        tool_path = rctx.path(tool)
        args = [
            tool_path,
            "download-blob",
            "--digest",
            digest,
            "--repository",
            repository,
            "--output",
            output,
        ] + [
            "--registry={}".format(registry)
            for registry in registries
        ]
        result = rctx.execute(args)
        if result.return_code != 0:
            fail("Failed to download blob: {}{}".format(result.stdout, result.stderr))
    else:
        fail("unknown downloader: {}".format(downloader))

    return struct(
        digest = digest,
        path = output,
        data = rctx.read(output) if wait_and_read else None,
        waiter = result if downloader == "bazel" else None,
    )

def download_manifest_rctx(rctx, *, downloader, reference, **kwargs):
    """Download a manifest from a container registry using Bazel's downloader.

    Args:
        rctx: Repository context.
        downloader: "img_tool" or "bazel".
        reference: The manifest reference to download.
        **kwargs: Additional arguments.

    Returns:
        A struct containing digest, path, and data of the downloaded manifest.
    """
    have_valid_digest = False
    registries = get_registries(rctx)
    if reference.startswith("sha256:"):
        have_valid_digest = True
        sha256 = reference.removeprefix("sha256:")
        kwargs["output"] = "blobs/sha256/" + sha256
    else:
        kwargs["output"] = "manifest.json"
        sha256 = None
    if have_valid_digest:
        maybe_existing = _check_existing_blob(rctx, reference)
        if maybe_existing != None:
            return maybe_existing
    return download_manifest(
        rctx,
        downloader = downloader,
        reference = reference,
        sha256 = sha256,
        have_valid_digest = have_valid_digest,
        repository = rctx.attr.repository,
        registries = registries,
        **kwargs
    )

def download_manifest(ctx, *, downloader, reference, sha256, have_valid_digest, repository, registries, **kwargs):
    """Download a manifest from a container registry using Bazel's downloader or img tool.

    Args:
        ctx: Repository context or module context.
        downloader: "img_tool" or "bazel".
        reference: The manifest reference to download.
        sha256: digest of the manifest (or None).
        have_valid_digest: bool indicating the presence of a valid digest.
        repository: Repository of the image (i.e. library/ubuntu)
        registries: List of registries that mirror the manifest.
        **kwargs: Additional arguments.

    Returns:
        A struct containing digest, path, and data of the downloaded manifest.
    """
    if downloader == "bazel":
        result = download_manifest_bazel(
            ctx,
            reference = reference,
            sha256 = sha256,
            have_valid_digest = have_valid_digest,
            repository = repository,
            registries = registries,
            **kwargs
        )
    else:
        # pull tool
        result = download_manifest_img_tool(
            ctx,
            reference = reference,
            sha256 = sha256,
            have_valid_digest = have_valid_digest,
            repository = repository,
            registries = registries,
        )

    if not have_valid_digest:
        fail("""Missing valid image digest. Observed the following digest when pulling manifest for {}:
    sha256:{}""".format(
            repository,
            result.sha256,
        ))
    return result

def download_manifest_bazel(rctx, *, reference, sha256, have_valid_digest, repository, registries, **kwargs):
    """Download a manifest from a container registry using Bazel's downloader.

    Args:
        rctx: Repository context.
        reference: The manifest reference to download.
        sha256: digest of the manifest (or None).
        have_valid_digest: bool indicating the presence of a valid digest.
        repository: Repository of the image (i.e. library/ubuntu)
        registries: List of registries that mirror the manifest.
        **kwargs: Additional arguments.

    Returns:
        A struct containing digest, path, and data of the downloaded manifest.
    """
    if have_valid_digest:
        kwargs["sha256"] = sha256
        kwargs["output"] = "blobs/sha256/" + sha256
    else:
        kwargs["output"] = "manifest.json"
    manifest_result = rctx.download(
        url = [
            "{protocol}://{registry}/v2/{repository}/manifests/{reference}".format(
                protocol = "https",
                registry = registry,
                repository = repository,
                reference = reference,
            )
            for registry in registries
        ],
        **kwargs
    )
    if have_valid_digest and manifest_result.sha256 != sha256:
        fail("expected manifest with digest sha256:{} but got sha256:{}".format(sha256, manifest_result.sha256))
    return struct(
        digest = reference if have_valid_digest else "sha256:" + manifest_result.sha256,
        path = kwargs["output"],
        data = rctx.read(kwargs["output"]),
        waiter = None,
    )

def download_manifest_img_tool(rctx, *, reference, sha256, have_valid_digest, repository, registries):
    """Download a manifest from a container registry using img tool.

    Args:
        rctx: Repository context.
        reference: The manifest reference to download.
        sha256: digest of the manifest (or None).
        have_valid_digest: bool indicating the presence of a valid digest.
        repository: Repository of the image (i.e. library/ubuntu)
        registries: List of registries that mirror the manifest.

    Returns:
        A struct containing digest, path, and data of the downloaded manifest.
    """
    tool = tool_for_repository_os(rctx)
    tool_path = rctx.path(tool)
    destination = "manifest.json"
    if have_valid_digest:
        destination = "blobs/sha256/" + sha256
    args = [
        tool_path,
        "download-manifest",
        "--repository",
        repository,
        "--output",
        destination,
    ] + [
        "--registry={}".format(registry)
        for registry in registries
    ]
    if have_valid_digest:
        args.extend(["--digest", "sha256:" + sha256])
    else:
        args.extend(["--tag", reference])

    result = rctx.execute(args)
    if result.return_code != 0:
        fail("Failed to download manifest: {}".format(result.stderr))
    return struct(
        digest = reference if have_valid_digest else None,
        path = destination,
        data = rctx.read(destination),
        waiter = None,
    )

def download_layers(rctx, downloader, digests):
    """Download all layers from a manifest.

    Args:
        rctx: Repository context.
        downloader: "img_tool" or "bazel".
        digests: A list of layer digests to download.

    Returns:
        A list of structs containing digest, path, and data of the downloaded layers.
    """
    downloaded_layers = []
    for digest in digests:
        layer_info = download_blob(rctx, downloader = downloader, digest = digest, wait_and_read = False)
        downloaded_layers.append(layer_info)
    for layer in downloaded_layers:
        if layer.waiter != None:
            layer.waiter.wait()
    return [downloaded_layer for downloaded_layer in downloaded_layers]

def download_with_tool(rctx, *, tool_path, reference):
    """Download an image using the img tool.

    Args:
        rctx: Repository context.
        tool_path: The path to the img tool to use for downloading.
        reference: The image reference to download.

    Returns:
        A struct containing manifest and layers of the downloaded image.
    """
    registries = get_registries(rctx)
    args = [
        tool_path,
        "pull",
        "--reference=" + reference,
        "--repository=" + rctx.attr.repository,
        "--layer-handling=" + rctx.attr.layer_handling,
    ] + ["--registry=" + r for r in registries]
    result = rctx.execute(args, quiet = False)
    if result.return_code != 0:
        fail("img tool failed with exit code {} and message {}".format(result.return_code, result.stderr))
