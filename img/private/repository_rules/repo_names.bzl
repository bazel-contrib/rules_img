"""Naming scheme of the repositories created by the images module extension.

The extension declares the repositories, and the BUILD files they generate refer to each other by
name, so both sides have to agree. Keeping every name in one place is what makes that checkable.

Keep the names short: they end up in filesystem paths, behind an already long canonical repository
name, and Windows still has a path length limit.

    img_<digest>            entry point of a pulled image; `:image` and `:original`
    orig_<digest>           unmodified index of a multi-platform image, behind `:original`
    m_<digest>_<position>   one child manifest of that index, keyed by the descriptor referring
                            to it, so two occurrences of one digest stay distinguishable and each
                            keeps the descriptor, sources and layer mode of its own parent
    blob_<digest>           a manifest, index, config or eagerly fetched layer blob
    lazy_<digest>           a layer blob fetched by a build action
"""

# Kinds of blob repository, and which target in them holds the blob.
MANIFEST_BLOB = "manifest"
BLOB = "blob"
LAZY_BLOB = "lazy"

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
    """Name of the repository holding a layer blob that a build action downloads.

    Args:
        digest: Digest of the blob.

    Returns:
        The repository name.
    """
    return "lazy_{}".format(_mangle(digest))

def image_repo_name(digest):
    """Name of the repository that is the entry point of a pulled image.

    Args:
        digest: Digest of the image (manifest or index).

    Returns:
        The repository name.
    """
    return "img_{}".format(_mangle(digest))

def original_repo_name(digest):
    """Name of the repository importing a pulled index exactly as it was pulled.

    Args:
        digest: Digest of the image index.

    Returns:
        The repository name.
    """
    return "orig_{}".format(_mangle(digest))

def child_repo_name(parent_digest, position):
    """Name of the repository importing one child manifest of a pulled index.

    Keyed by the descriptor that refers to the manifest, not by the manifest's own digest: an
    index may list the same digest twice with different descriptors, and two images sharing a
    child may want different layer modes or have different registries to fetch it from.

    Args:
        parent_digest: Digest of the index listing the manifest.
        position: Position of the descriptor within that index.

    Returns:
        The repository name.
    """
    return "m_{}_{}".format(_mangle(parent_digest), position)

def blob_label(digest, kind):
    """Label of a blob in the repository holding it.

    Args:
        digest: Digest of the blob.
        kind: One of `MANIFEST_BLOB`, `BLOB` or `LAZY_BLOB`.

    Returns:
        The label, as a string.
    """
    if kind == MANIFEST_BLOB:
        return "@{}//:manifest.json".format(blob_repo_name(digest))
    if kind == BLOB:
        return "@{}//:blob".format(blob_repo_name(digest))
    if kind == LAZY_BLOB:
        return "@{}//:blob".format(lazy_blob_repo_name(digest))
    fail("unknown blob kind: {}".format(kind))

def image_label(repo_name):
    """Label of the image target of an image, original or child manifest repository.

    Args:
        repo_name: Name of the repository.

    Returns:
        The label, as a string.
    """
    return "@{}//:image".format(repo_name)
