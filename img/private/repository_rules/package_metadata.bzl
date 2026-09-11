"""The package metadata the repositories of a pulled image carry.

A pulled image is spread over several repositories - the one it is referred to through, the one
holding the unmodified index, and one per child manifest - and every one of them describes the
same pull. Rendering that description in one place is what keeps the purl of the entry point
naming the digest the user pinned, rather than whichever manifest a build happened to select.
"""

# The load statement `package_metadata_target` needs, for a BUILD file that has none yet.
PACKAGE_METADATA_LOAD = 'load("@package_metadata//rules:package_metadata.bzl", "package_metadata")\n'

# Attaches the metadata below to every target of the repository.
REPO_FILE = """\
repo(
    default_package_metadata = ["//:package_metadata"],
)
"""

_PACKAGE_METADATA = """
package_metadata(
    name = "package_metadata",
    purl = {purl},
    visibility = ["//:__subpackages__"],
)
"""

def image_purl(repository, identifier, registries):
    """Build the package URL of a pulled image.

    Args:
        repository: The image repository, as pulled.
        identifier: Digest of the image, or its tag when no digest is known.
        registries: Registries the image can be pulled from. Only recorded when there is exactly
            one of them, because a purl names a single location.

    Returns:
        The purl, as a string.
    """
    purl = "pkg:docker/{}@{}".format(repository, identifier)
    if len(registries) == 1:
        purl += "?repository_url={}".format(registries[0])
    return purl

def package_metadata_target(purl):
    """Render the `package_metadata` target `REPO_FILE` refers to.

    Args:
        purl: Package URL of the image the repository describes.

    Returns:
        BUILD file text declaring the target.
    """
    return _PACKAGE_METADATA.format(purl = repr(purl))
