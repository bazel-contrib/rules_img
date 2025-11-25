"""Helper functions for registry handling."""

def get_registries(rctx):
    """Get the list of registries from repository rule attributes.

    This function consolidates registry attribute handling with proper defaults.
    It checks both the singular 'registry' and plural 'registries' attributes,
    and provides a sensible default (index.docker.io) if neither is specified.

    Args:
        rctx: Repository context with 'registry' and 'registries' attributes.

    Returns:
        A list of registry strings to try in order.
    """
    registries = []
    if rctx.attr.registry:
        registries.append(rctx.attr.registry)
    if len(rctx.attr.registries) > 0:
        registries.extend(rctx.attr.registries)
    if len(registries) == 0:
        # default to Docker Hub.
        # This exists mostly for compatibility with rules_oci.
        registries.append("index.docker.io")
    return registries
