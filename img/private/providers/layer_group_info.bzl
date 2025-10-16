"""Defines LayerGroup provider for outputting multiple layers from a single rule."""

DOC = """\
Information about an ordered group of layers as components of a container image.
"""


FIELDS = dict(
    layers = "List of LayerInfo providers representing the layers in this group, in order.",
)

LayerGroupInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
