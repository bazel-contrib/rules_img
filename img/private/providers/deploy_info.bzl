"""Defines providers for the push, load, and deploy rules."""

DOC = """\
Information required to push or load an image or image index to a registry or
container runtime.
"""

FIELDS = dict(
    image = "ImageManifestInfo or ImageIndexInfo of the image or image index to push or load.",
    deploy_manifest = "File containing the deploy manifest (JSON).",
    layer_hints = "File containing layer path hints (or None).",
    include_layers = "Bool, whether layer blobs must be present in the run-time file tree (True for eager strategies, False for lazy/CAS strategies).",
)

DeployInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
