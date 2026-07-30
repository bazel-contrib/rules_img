"""Defines the PushConfigInfo provider for push configuration without an image reference."""

DOC = """\
Push configuration for deploying images to a registry. Captures registry,
repository, tags, and strategy without referencing a specific image.
"""

FIELDS = dict(
    registry = "Registry URL template string.",
    repository = "Repository template string.",
    tags = "List of tag template strings (combined from tag/tag_list).",
    manifest_tags = "Per-platform tag template strings for multi-platform pushes.",
    tag_file = "File with newline-delimited tags, or None.",
    destination_file = "File containing {registry}/{repository}, or None.",
    referrers = "List of structs(manifest_info, index_info) for referrer pushes.",
    cross_mount_from = "Target providing DeployInfo for cross-mounting, or None.",
    strategy = "Resolved push strategy string (never 'auto').",
    cross_mount_strategy = "Resolved cross-mount strategy string.",
    build_settings = "Dict(string, string) of resolved build setting values.",
    stamp = "Stamp preference string ('auto', 'force', 'disabled').",
    stamp_settings = "StampSettingInfo provider for stamp resolution.",
    tracks_content = "Bool: when True, expose the image digest to templates and re-stamp tags on content change.",
    signing = "struct(config_info, best_effort, targets) describing how to sign this push, or None.",
    blob_repository = "Resolved staging repository that image blobs are pushed to and cross-mounted from. At build time every blob (layers and config) is staged here; layers are cross-mounted into the image's real repository. Empty means blobs go to the image's own repository.",
    forbid_layer_push = "Bool: when True, `img deploy` refuses to upload layer blob bytes (layers must be cross-mounted or already present).",
    push_at_build_time_mode = "Resolved push-at-build-time mode: 'disabled', 'best_effort', or 'enabled'.",
    push_at_build_time_content = "Resolved push-at-build-time content: 'blobs' or 'blobs_and_manifests'.",
    push_at_build_time_manifest_repository = "Resolved repository the build-time manifest push uploads manifest(s)/index and config to instead of the operation's own repository, or ''. Does not affect blob cross-mounting.",
    push_at_build_time_exec_properties = "Dict(string, string) of execution_requirements forwarded to the PushImage build-time push actions.",
    push_at_build_time_gateway = "Shared OCI distribution gateway endpoint for the build-time push actions, or ''.",
    push_at_build_time_push_gateway = "Push OCI distribution gateway endpoint for the build-time push actions, or ''.",
    push_at_build_time_pull_gateway = "Pull OCI distribution gateway endpoint for the build-time push actions, or ''.",
    insecure = "Bool: when True, the build-time push actions address registries over plain HTTP and accept untrusted TLS certificates (IMG_INSECURE).",
)

PushConfigInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
