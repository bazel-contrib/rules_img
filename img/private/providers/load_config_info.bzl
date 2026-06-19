"""Defines the LoadConfigInfo provider for load configuration without an image reference."""

DOC = """\
Load configuration for importing images into a container daemon. Captures
daemon, tags, and strategy without referencing a specific image.
"""

FIELDS = dict(
    daemon = "Resolved daemon string (never 'auto').",
    tags = "List of tag template strings (combined from tag/tag_list).",
    tag_file = "File with newline-delimited tags, or None.",
    strategy = "Resolved load strategy string (never 'auto').",
    build_settings = "Dict(string, string) of resolved build setting values.",
    stamp = "Stamp preference string ('auto', 'force', 'disabled').",
    stamp_settings = "StampSettingInfo provider for stamp resolution.",
    tracks_content = "Bool: when True, expose the image digest to templates and re-stamp tags on content change.",
)

LoadConfigInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
