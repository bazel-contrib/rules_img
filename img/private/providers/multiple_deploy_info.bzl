"""Defines providers for deploying multiple operations together."""

DOC = """\
A collection of deploy operations that are deployed together.

Consumed by multi_deploy, which expands the contained DeployInfo providers in
place, as if each had been listed in `operations` individually.

Unlike the other inputs of multi_deploy, this provider may be empty:
an unset or empty `infos` is allowed, leaving a multi_deploy with
nothing to deploy rather than failing.
"""

FIELDS = dict(
    infos = "depset of DeployInfo to be deployed together, in traversal order. May be unset or empty.",
)

MultipleDeployInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
