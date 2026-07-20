"""Provider for push-at-build-time settings."""

DOC = """\
Collection of active push-at-build-time settings.
"""

FIELDS = dict(
    mode = "One of 'disabled', 'best_effort', or 'enabled'.",
    content = "One of 'blobs' or 'blobs_and_manifests'.",
    gateway = "Shared OCI distribution gateway endpoint (IMG_REGISTRY_GATEWAY), or '' if unset. Fallback for both push and pull.",
    push_gateway = "Push OCI distribution gateway endpoint (IMG_REGISTRY_PUSH_GATEWAY), or '' if unset.",
    pull_gateway = "Pull OCI distribution gateway endpoint (IMG_REGISTRY_PULL_GATEWAY), or '' if unset.",
)

PushAtBuildTimeSettingsInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
