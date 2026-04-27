"""Defines providers for a collection of image layers."""

DOC = """\
An ordered collection of layers as components of a container image.

Layers are sorted from bottom (base) to top (application).
"""

FIELDS = dict(
    layers = "Iterable of SingleLayerInfo providers.",
)

LayersInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
