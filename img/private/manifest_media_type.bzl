"""Helper for inferring manifest mediaType."""

MEDIA_TYPE_INDEX = "application/vnd.oci.image.index.v1+json"
MEDIA_TYPE_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
DOCKER_MANIFEST_LIST_V2 = "application/vnd.docker.distribution.manifest.list.v2+json"
DOCKER_MANIFEST_V2 = "application/vnd.docker.distribution.manifest.v2+json"

def get_media_type(manifest):
    """Get the mediaType of a manifest, inferring if missing.

    Args:
        manifest: A dict representing a manifest or index blob

    Returns:
        The mediaType as a string, either from the "mediaType" field or
        inferred from the manifest structure (presence of "config" or "manifests")
    """
    media_type = manifest.get("mediaType")
    if media_type == None:
        # Infer mediaType based on structure
        if "config" in manifest:
            media_type = MEDIA_TYPE_MANIFEST
        elif "manifests" in manifest:
            media_type = MEDIA_TYPE_INDEX
    return media_type

def kind(media_type):
    """Get the kind of manifest based on mediaType.

    Args:
        media_type: The mediaType string

    Returns:
        "manifest" for single-platform manifests, "index" for multi-platform indexes,
        or "unknown" if the mediaType is unrecognized.
    """
    if media_type in [MEDIA_TYPE_MANIFEST, DOCKER_MANIFEST_V2]:
        return "manifest"
    elif media_type in [MEDIA_TYPE_INDEX, DOCKER_MANIFEST_LIST_V2]:
        return "index"
    else:
        return "unknown"
