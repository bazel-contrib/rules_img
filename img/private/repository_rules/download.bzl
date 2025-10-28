"""Repository rules for downloading container image components."""

def setup_blob_files(rctx, blob_files):
    """Set up pre-downloaded blob files by creating symlinks.

    Args:
        rctx: Repository context.
        blob_files: Dictionary mapping blob digests to file labels.
    """
    if len(blob_files) == 0:
        return

    # Create blobs directory structure by creating a marker file
    # This ensures the directory exists on all platforms
    rctx.file("blobs/sha256/.marker", content = "", executable = False)

    # Create symlinks for blob files
    for digest, label in blob_files.items():
        file_path = rctx.path(label)

        # Watch the file for changes
        rctx.watch(file_path)

        # Create symlink to the blob file
        sha256 = digest.removeprefix("sha256:")
        blob_path = "blobs/sha256/{}".format(sha256)
        rctx.symlink(file_path, blob_path)

    rctx.delete("blobs/sha256/.marker")

def download_blob(rctx, *, digest, wait_and_read = True, **kwargs):
    """Download a blob from a container registry using Bazel's downloader.

    Args:
        rctx: Repository context.
        digest: The blob digest to download.
        wait_and_read: If True, wait for the download to complete and read the data.
                       If False, return a waiter that can be used to wait for the download.
        **kwargs: Additional arguments.

    Returns:
        A struct containing digest, path, and data of the downloaded blob.
    """
    sha256 = digest.removeprefix("sha256:")
    output = "blobs/sha256/" + sha256

    # Check if blob already exists (from blob_files)
    if rctx.path(output).exists:
        return struct(
            digest = digest,
            path = output,
            data = rctx.read(output) if wait_and_read else None,
            waiter = None,
        )

    # In airgapped mode, fail if blob is not available locally
    if rctx.attr.airgapped:
        fail("Blob {} not available locally in airgapped mode".format(digest))

    registries = [r for r in rctx.attr.registries]
    if rctx.attr.registry:
        registries.append(rctx.attr.registry)
    if len(registries) == 0:
        fail("need at least one registry to pull from")
    result = rctx.download(
        url = [
            "{protocol}://{registry}/v2/{repository}/blobs/{digest}".format(
                protocol = "https",
                registry = registry,
                repository = rctx.attr.repository,
                digest = digest,
            )
            for registry in registries
        ],
        sha256 = sha256,
        output = output,
        block = wait_and_read,
        **kwargs
    )
    return struct(
        digest = digest,
        path = output,
        data = rctx.read(output) if wait_and_read else None,
        waiter = result,
    )

def download_manifest(rctx, *, reference, **kwargs):
    """Download a manifest from a container registry using Bazel's downloader.

    Args:
        rctx: Repository context.
        reference: The manifest reference to download.
        **kwargs: Additional arguments.

    Returns:
        A struct containing digest, path, and data of the downloaded manifest.
    """
    have_valid_digest = False
    registries = [r for r in rctx.attr.registries]
    if rctx.attr.registry:
        registries.append(rctx.attr.registry)
    if len(registries) == 0:
        fail("need at least one registry to pull from")
    if reference.startswith("sha256:"):
        have_valid_digest = True
        sha256 = reference.removeprefix("sha256:")
        kwargs["sha256"] = sha256
        kwargs["output"] = "blobs/sha256/" + sha256

        # Check if manifest already exists (from blob_files)
        if rctx.path(kwargs["output"]).exists:
            return struct(
                digest = reference,
                path = kwargs["output"],
                data = rctx.read(kwargs["output"]),
            )

        # In airgapped mode, fail if manifest is not available locally
        if rctx.attr.airgapped:
            fail("Manifest {} not available locally in airgapped mode".format(reference))
    else:
        kwargs["output"] = "manifest.json"

        # Without a digest, we can't use cached blobs and airgapped mode doesn't make sense
        if rctx.attr.airgapped:
            fail("Airgapped mode requires a valid digest (sha256:...), but got tag reference: {}".format(reference))

    manifest_result = rctx.download(
        url = [
            "{protocol}://{registry}/v2/{repository}/manifests/{reference}".format(
                protocol = "https",
                registry = registry,
                repository = rctx.attr.repository,
                reference = reference,
            )
            for registry in registries
        ],
        **kwargs
    )
    if not have_valid_digest:
        fail("""Missing valid image digest. Observed the following digest when pulling manifest for {}:
    sha256:{}""".format(
            rctx.attr.repository + ":" + rctx.attr.tag,
            manifest_result.sha256,
        ))
    return struct(
        digest = reference,
        path = kwargs["output"],
        data = rctx.read(kwargs["output"]),
    )

def download_layers(rctx, digests):
    """Download all layers from a manifest.

    Args:
        rctx: Repository context.
        digests: A list of layer digests to download.

    Returns:
        A list of structs containing digest, path, and data of the downloaded layers.
    """
    downloaded_layers = []
    for digest in digests:
        layer_info = download_blob(rctx, digest = digest, wait_and_read = False)
        downloaded_layers.append(layer_info)
    for layer in downloaded_layers:
        # Only wait if there's a waiter (blob was downloaded, not cached)
        if layer.waiter != None:
            layer.waiter.wait()
    return [downloaded_layer for downloaded_layer in downloaded_layers]

def get_blob(rctx, *, digest, read = True, **kwargs):
    """Obtain a blob from a container registry.

    Args:
        rctx: Repository context.
        digest: The blob digest to download.
        read: If True, read the data from disk after downloading.
        **kwargs: Additional arguments.

    Returns:
        A struct containing digest, path, and data of the downloaded blob.
    """
    if rctx.attr.downloader == "bazel":
        # Use Bazel's downloader to download the blob now
        return download_blob(rctx, digest = digest, wait_and_read = read, **kwargs)

    # When using the img tool, the data already exists on disk
    # so just read it from there
    path = "blobs/sha256/" + digest.removeprefix("sha256:")
    return struct(
        digest = digest,
        path = path,
        data = rctx.read(path) if read else None,
    )

def get_manifest(rctx, *, reference, **kwargs):
    """Obtain a manifest from a container registry.

    Args:
        rctx: Repository context.
        reference: The manifest reference to download.
        **kwargs: Additional arguments.

    Returns:
        A struct containing digest, path, and data of the downloaded manifest.
    """
    if rctx.attr.downloader == "bazel":
        # Use Bazel's downloader to download the manifest now
        return download_manifest(rctx, reference = reference, **kwargs)

    # When using the img tool, the data already exists on disk
    # so just read it from there
    path = "blobs/sha256/" + reference.removeprefix("sha256:")
    return struct(
        digest = reference,
        path = path,
        data = rctx.read(path),
    )

def get_layers(rctx, digests):
    """Obtain all layers from a manifest.

    Args:
        rctx: Repository context.
        digests: A list of layer digests to download.

    Returns:
        A list of structs containing digest, path, and data of the downloaded layers.
    """
    if rctx.attr.downloader == "bazel":
        # Use Bazel's downloader to download the layers now
        return download_layers(rctx, digests = digests)

    # When using the img tool, the data already exists on disk
    # so just read it from there
    return [
        struct(
            digest = digest,
            path = "blobs/sha256/" + digest.removeprefix("sha256:"),
        )
        for digest in digests
    ]

def download_with_tool(rctx, *, tool_path, reference, airgapped = False):
    """Download an image using the img tool.

    Args:
        rctx: Repository context.
        tool_path: The path to the img tool to use for downloading.
        reference: The image reference to download.
        airgapped: Enable airgapped mode (no network access).

    Returns:
        A struct containing manifest and layers of the downloaded image.
    """
    registries = [r for r in rctx.attr.registries]
    if rctx.attr.registry:
        registries.append(rctx.attr.registry)

    args = [
        tool_path,
        "pull",
        "--reference=" + reference,
        "--repository=" + rctx.attr.repository,
        "--layer-handling=" + rctx.attr.layer_handling,
    ] + ["--registry=" + r for r in registries]
    if airgapped:
        args.append("--airgapped")
    result = rctx.execute(args, quiet = False)
    if result.return_code != 0:
        fail("img tool failed with exit code {} and message {}".format(result.return_code, result.stderr))
