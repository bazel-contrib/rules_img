"""Defines providers for the push, load, and deploy rules."""

DOC = """\
Information required to push or load an image or image index to a registry or
container runtime.
"""

FIELDS = dict(
    image = "ImageManifestInfo or ImageIndexInfo of the image or image index to push or load.",
    deploy_manifest = "File containing the deploy manifest (JSON).",
    layer_hints = "File containing layer path hints (or None).",
)

DeployInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
