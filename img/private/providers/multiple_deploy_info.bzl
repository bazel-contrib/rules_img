"""Defines providers for deploying multiple operations together."""

DOC = """\
A collection of deploy operations that are deployed together.

Consumed by multi_deploy, which expands the contained DeployInfo providers in
place, as if each had been listed in `operations` individually.
"""

FIELDS = dict(
    infos = "depset of DeployInfo to be deployed together, in traversal order.",
)

MultipleDeployInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
