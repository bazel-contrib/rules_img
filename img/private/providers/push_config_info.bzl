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
    blob_repository = "When non-empty, the staging repository that image blobs are pushed to and cross-mounted from. At build time every blob (layers and config) is staged here; layers are cross-mounted into the image's real repository. Empty means blobs go to the image's own repository.",
    forbid_layer_push = "Bool: when True, `img deploy` refuses to upload layer blob bytes (layers must be cross-mounted or already present).",
)

PushConfigInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
