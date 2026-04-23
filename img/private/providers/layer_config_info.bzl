"""Defines providers for layers that contribute to the config blob of a manifest (like layer_from_binary)."""

DOC = """\
Information about configuration fragments provided by a layer.
If passed as a layer to `image_manifest`, this information will be included in the image config.
"""

FIELDS = dict(
    entrypoint = "List of strings to be used as the entrypoint field in the image config (or None).",
    cmd = "List of strings to be used as the Cmd field of the image (or None).",
    env = "Environment variables that shall be added to the image config (or None).",
    working_dir = "WorkingDir in the image config (or None).",
)

ImageLayerConfigInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
