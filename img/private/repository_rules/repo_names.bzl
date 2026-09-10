"""Naming scheme of the repositories created by the images module extension.

The extension declares the repositories and the repository rules refer to each other by name,
so both sides have to agree. Keep the names short: they end up in filesystem paths, and the
canonical repository name is already long.
"""

def _mangle(digest):
    return digest.replace("sha256:", "").replace(":", "_")

def blob_repo_name(digest):
    """Name of the repository holding an eagerly fetched blob.

    Args:
        digest: Digest of the blob.

    Returns:
        The repository name.
    """
    return "blob_{}".format(_mangle(digest))

def lazy_blob_repo_name(digest):
    """Name of the repository holding a layer blob that is downloaded by a build action.

    Args:
        digest: Digest of the blob.

    Returns:
        The repository name.
    """
    return "lazy_{}".format(_mangle(digest))

def image_repo_name(digest):
    """Name of the repository of a pulled image.

    Args:
        digest: Digest of the image (manifest or index).

    Returns:
        The repository name.
    """
    return "img_{}".format(_mangle(digest))

def manifest_repo_name(digest):
    """Name of the repository of a single child manifest of a pulled image index.

    Args:
        digest: Digest of the manifest.

    Returns:
        The repository name.
    """
    return "imgm_{}".format(_mangle(digest))

def blob_label(digest):
    """Label of a blob (config or layer) in its own repository.

    Args:
        digest: Digest of the blob.

    Returns:
        The label, as a string.
    """
    return "@{}//:blob".format(blob_repo_name(digest))

def lazy_blob_label(digest):
    """Label of a lazily downloaded layer blob in its own repository.

    Args:
        digest: Digest of the blob.

    Returns:
        The label, as a string.
    """
    return "@{}//:blob".format(lazy_blob_repo_name(digest))

def manifest_blob_label(digest):
    """Label of a manifest or index blob in its own repository.

    Args:
        digest: Digest of the manifest or index.

    Returns:
        The label, as a string.
    """
    return "@{}//:manifest.json".format(blob_repo_name(digest))

def manifest_target_label(digest):
    """Label of the target importing a single child manifest of an image index.

    Args:
        digest: Digest of the manifest.

    Returns:
        The label, as a string.
    """
    return "@{}//:image".format(manifest_repo_name(digest))
