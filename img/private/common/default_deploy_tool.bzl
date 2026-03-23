"""Utilities for resolving the default deploy tool."""

def default_deploy_tool(tool_cfg):
    """Returns the label for the deploy tool based on the given configuration.

    This is a computed default for the _deploy_tool attribute.

    Args:
        tool_cfg: A string indicating the deploy tool configuration.
            "host" for the host platform tool, "target" for the target
            platform tool.
    Returns:
        A Label pointing to the resolved deploy tool.
    """
    if tool_cfg == "host":
        return Label("//img/deploy_tool/for_host")
    elif tool_cfg == "target":
        return Label("//img/deploy_tool")
    fail("Invalid tool_cfg: {}".format(tool_cfg))
