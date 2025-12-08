"""Repository rules for downloading container image components."""

load("@pull_hub_repo//:defs.bzl", "tool_for_repository_os")
load(":registry.bzl", "get_registries", "get_sources_list")

def learn_digest_from_tag(rctx, *, tag, downloader, sources):
    """Learn the digest of an image from its tag by downloading manifest headers.

    Args:
        rctx: Repository context.
        tag: The tag to resolve.
        downloader: "img_tool" or "bazel".
        sources: Sources dict mapping repositories to registries.

    Returns:
        The resolved digest as a string (e.g., "sha256:abc123...") or None if resolution failed.
    """
    if downloader == "bazel":
        # Use Bazel's download to get the manifest and extract its digest
        # Build URLs from all sources
        urls = []
        for repository, registries in sources.items():
            for registry in registries:
                urls.append(
                    "https://{registry}/v2/{repository}/manifests/{tag}".format(
                        registry = registry,
                        repository = repository,
                        tag = tag,
                    ),
                )

        result = rctx.download(
            url = urls,
            output = "temp_manifest_for_digest_learning.json",
        )

        # The digest is the SHA256 of the downloaded manifest
        return "sha256:" + result.sha256
    else:
        # Use img_tool download-manifest command with --print-digest flag
        tool = tool_for_repository_os(rctx)
        tool_path = rctx.path(tool)

        # Convert sources dict to list format
        sources_list = get_sources_list(sources)

        args = [
            tool_path,
            "download-manifest",
            "--tag",
            tag,
            "--print-digest",
        ] + [
            "--source={}".format(source)
            for source in sources_list
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

def download_blob(rctx, *, downloader, digest, sources, wait_and_read = True, output = None, **kwargs):
    """Download a blob from a container registry using the specified downloader.

    Args:
        rctx: Repository context or module context.
        downloader: "img_tool" or "bazel".
        digest: The blob digest to download.
        sources: Sources dict mapping repositories to registries.
        wait_and_read: If True, wait for the download to complete and read the data.
                       If False, return a waiter that can be used to wait for the download.
        output: Optional output path for the downloaded blob. If not specified, defaults to "blobs/sha256/<sha256>".
        **kwargs: Additional arguments.

    Returns:
        A struct containing digest, path, and data of the downloaded blob.
    """
    sha256 = digest.removeprefix("sha256:")
    if output == None:
        output = "blobs/sha256/" + sha256

    # Only check for existing blob in default location if using default output path
    if output == "blobs/sha256/" + sha256:
        maybe_existing = _check_existing_blob(rctx, digest, wait_and_read)
        if maybe_existing != None:
            return maybe_existing
    if downloader == "bazel":
        # Build URLs from all sources
        urls = []
        for repository, registries in sources.items():
            for registry in registries:
                urls.append(
                    "{protocol}://{registry}/v2/{repository}/blobs/{digest}".format(
                        protocol = "https",
                        registry = registry,
                        repository = repository,
                        digest = digest,
                    ),
                )

        result = rctx.download(
            url = urls,
            sha256 = sha256,
            output = output,
            block = wait_and_read,
            **kwargs
        )
    elif downloader == "img_tool":
        tool = tool_for_repository_os(rctx)
        tool_path = rctx.path(tool)

        # Convert sources dict to list format
        sources_list = get_sources_list(sources)

        args = [
            tool_path,
            "download-blob",
            "--digest",
            digest,
            "--output",
            output,
        ] + [
            "--source={}".format(source)
            for source in sources_list
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

def download_blob_from_sources(rctx, *, downloader, digest, wait_and_read = True, **kwargs):
    """Download a blob using the sources attribute from rctx.

    Args:
        rctx: Repository context with 'sources' attribute.
        downloader: "img_tool" or "bazel".
        digest: The blob digest to download.
        wait_and_read: If True, wait for the download to complete and read the data.
        **kwargs: Additional arguments passed to download_blob.

    Returns:
        A struct containing digest, path, and data of the downloaded blob.
    """
    return download_blob(
        rctx,
        downloader = downloader,
        digest = digest,
        sources = rctx.attr.sources,
        wait_and_read = wait_and_read,
        **kwargs
    )

def download_manifest_rctx(rctx, *, downloader, reference, **kwargs):
    """Download a manifest from a container registry (without support for multi-source).

    Args:
        rctx: Repository context with 'repository', 'registry', and 'registries' attributes.
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

    # Build sources from legacy attrs
    sources = {rctx.attr.repository: registries}
    return download_manifest(
        rctx,
        downloader = downloader,
        reference = reference,
        sha256 = sha256,
        have_valid_digest = have_valid_digest,
        sources = sources,
        **kwargs
    )

def download_manifest_from_sources(rctx, *, downloader, reference, **kwargs):
    """Download a manifest using the sources attribute from rctx.

    Args:
        rctx: Repository context with 'sources' attribute.
        downloader: "img_tool" or "bazel".
        reference: The manifest reference to download (tag or digest).
        **kwargs: Additional arguments passed to download_manifest.

    Returns:
        A struct containing digest, path, and data of the downloaded manifest.
    """
    have_valid_digest = False
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
        sources = rctx.attr.sources,
        **kwargs
    )

def download_manifest(ctx, *, downloader, reference, sha256, have_valid_digest, sources, **kwargs):
    """Download a manifest from a container registry using Bazel's downloader or img tool.

    Args:
        ctx: Repository context or module context.
        downloader: "img_tool" or "bazel".
        reference: The manifest reference to download.
        sha256: digest of the manifest (or None).
        have_valid_digest: bool indicating the presence of a valid digest.
        sources: Sources dict mapping repositories to registries.
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
            sources = sources,
            **kwargs
        )
    else:
        # pull tool
        result = download_manifest_img_tool(
            ctx,
            reference = reference,
            sha256 = sha256,
            have_valid_digest = have_valid_digest,
            sources = sources,
        )

    if not have_valid_digest:
        # Get first repository from sources for error message
        repository = sources.keys()[0] if sources else "unknown"
        fail("""Missing valid image digest. Observed the following digest when pulling manifest for {}:
    sha256:{}""".format(
            repository,
            result.sha256,
        ))
    return result

def download_manifest_bazel(rctx, *, reference, sha256, have_valid_digest, sources, **kwargs):
    """Download a manifest from a container registry using Bazel's downloader.

    Args:
        rctx: Repository context.
        reference: The manifest reference to download.
        sha256: digest of the manifest (or None).
        have_valid_digest: bool indicating the presence of a valid digest.
        sources: Sources dict mapping repositories to registries.
        **kwargs: Additional arguments.

    Returns:
        A struct containing digest, path, and data of the downloaded manifest.
    """
    if have_valid_digest:
        kwargs["sha256"] = sha256
        kwargs["output"] = "blobs/sha256/" + sha256
    else:
        kwargs["output"] = "manifest.json"

    # Build URLs from all sources
    urls = []
    for repository, registries in sources.items():
        for registry in registries:
            urls.append(
                "{protocol}://{registry}/v2/{repository}/manifests/{reference}".format(
                    protocol = "https",
                    registry = registry,
                    repository = repository,
                    reference = reference,
                ),
            )

    manifest_result = rctx.download(
        url = urls,
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

def download_manifest_img_tool(rctx, *, reference, sha256, have_valid_digest, sources):
    """Download a manifest from a container registry using img tool.

    Args:
        rctx: Repository context.
        reference: The manifest reference to download.
        sha256: digest of the manifest (or None).
        have_valid_digest: bool indicating the presence of a valid digest.
        sources: Sources dict mapping repositories to registries.

    Returns:
        A struct containing digest, path, and data of the downloaded manifest.
    """
    tool = tool_for_repository_os(rctx)
    tool_path = rctx.path(tool)
    destination = "manifest.json"
    if have_valid_digest:
        destination = "blobs/sha256/" + sha256

    # Convert sources dict to list format
    sources_list = get_sources_list(sources)

    args = [
        tool_path,
        "download-manifest",
        "--output",
        destination,
    ] + [
        "--source={}".format(source)
        for source in sources_list
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

def download_layers(rctx, downloader, digests, sources):
    """Download all layers from a manifest.

    Args:
        rctx: Repository context.
        downloader: "img_tool" or "bazel".
        digests: A list of layer digests to download.
        sources: Sources dict mapping repositories to registries.

    Returns:
        A list of structs containing digest, path, and data of the downloaded layers.
    """
    downloaded_layers = []
    for digest in digests:
        layer_info = download_blob(rctx, downloader = downloader, digest = digest, sources = sources, wait_and_read = False)
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
