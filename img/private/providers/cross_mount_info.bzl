"""Defines provider for cross-repository mount hints during push."""

DOC = """\
Information about a repository from which layers can be cross-mounted during push.

When pushing to a registry, layers that already exist in another repository on the
same registry can be "mounted" (copied server-side) instead of being re-uploaded.
"""

FIELDS = dict(
    registry = "Registry where the layers are available (e.g. 'gcr.io').",
    repository = "Repository path where the layers are available (e.g. 'my-project/base').",
)

CrossMountInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
