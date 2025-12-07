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

def get_repository_and_registries_from_sources(rctx):
    """Extract repository and registries from the sources attribute.

    The sources attribute is a string_list_dict where:
    - Keys are repository names (e.g., "library/ubuntu")
    - Values are lists of registries that serve that repository

    This function enforces a single repository entry for backwards compatibility.
    For multi-source support, use get_sources_list() instead.

    Args:
        rctx: Repository context with 'sources' attribute.

    Returns:
        A struct with 'repository' (string) and 'registries' (list of strings).

    Fails:
        If sources has more than one repository or is empty.
    """
    if not hasattr(rctx.attr, "sources") or not rctx.attr.sources:
        fail("sources attribute must be specified and non-empty")

    if len(rctx.attr.sources) != 1:
        fail("sources must contain exactly one repository, got {}".format(len(rctx.attr.sources)))

    # Extract the single repository and its registries
    repository = rctx.attr.sources.keys()[0]
    registries = rctx.attr.sources[repository]

    if len(registries) == 0:
        # default to Docker Hub if no registries specified
        registries = ["index.docker.io"]

    return struct(
        repository = repository,
        registries = registries,
    )

def get_sources_list(sources_dict):
    """Convert sources dict to a list of source entries for command-line passing.

    Args:
        sources_dict: A string_list_dict where keys are repositories and values are registry lists.

    Returns:
        A list of "repository@registry" strings for all source combinations.
        Repositories with empty registry lists default to "index.docker.io".
    """
    source_list = []
    for repository, registries in sources_dict.items():
        if len(registries) == 0:
            # Default to Docker Hub if no registries specified
            registries = ["index.docker.io"]
        for registry in registries:
            source_list.append("{}@{}".format(repository, registry))
    return source_list
