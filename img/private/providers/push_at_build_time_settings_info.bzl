"""Provider for push-at-build-time settings."""

DOC = """\
Collection of active push-at-build-time settings.
"""

FIELDS = dict(
    mode = "One of 'disabled', 'best_effort', or 'enabled'.",
    content = "One of 'blobs' or 'blobs_and_manifests'.",
    manifest_repository = "Repository the build-time manifest push (content='blobs_and_manifests') uploads manifest(s)/index and config to instead of the image's real repository, or '' to use the real repository. Does not affect blob cross-mounting.",
    gateway = "Shared OCI distribution gateway endpoint (IMG_REGISTRY_GATEWAY), or '' if unset. Fallback for both push and pull.",
    push_gateway = "Push OCI distribution gateway endpoint (IMG_REGISTRY_PUSH_GATEWAY), or '' if unset.",
    pull_gateway = "Pull OCI distribution gateway endpoint (IMG_REGISTRY_PULL_GATEWAY), or '' if unset.",
)

PushAtBuildTimeSettingsInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
