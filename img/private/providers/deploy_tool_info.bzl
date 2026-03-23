"""Defines providers for tools used during deployment."""

DOC = """\
Information about tools used during deployment.
"""

FIELDS = dict(
    img_deploy_exe = "The img deploy tool executable.",
    launcher_template = "The hermetic-launcher template executable.",
)

DeployToolInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
