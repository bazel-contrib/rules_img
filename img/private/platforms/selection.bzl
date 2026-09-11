"""Generating the `config_setting`s that route a pulled image to one manifest per platform.

`selection_configs` turns the platforms an image offers into a set of pairwise disjoint constraint
combinations, each pointing at the position of the manifest `match_platform_index` picks for it.
The generated `select()` therefore reproduces, during the loading phase, exactly the choice
`select_base` would make during analysis - which is what lets a build fetch only the manifest,
config and layers of the platform it builds for.

The constraint values come from `//img/private/platforms:tables.bzl`, the same table
`//img/private/config:target_os_cpu` reads the target platform through.
"""

load(":match.bzl", "match_platform_index")
load(":tables.bzl", "CPU_CONSTRAINTS", "EMPTY_VARIANT_CONSTRAINT", "OS_CONSTRAINTS", "VARIANT_CONSTRAINTS_BY_ARCHITECTURE")

# Resolved to canonical labels here, because they end up in the BUILD file of a generated
# repository, which has a repository mapping of its own.
_OS = sorted([(str(Label(constraint)), value) for (constraint, value) in OS_CONSTRAINTS.items()])
_CPU = sorted([(str(Label(constraint)), value) for (constraint, value) in CPU_CONSTRAINTS.items()])
_EMPTY_VARIANT = (str(Label(EMPTY_VARIANT_CONSTRAINT)), "")
_VARIANTS = {
    architecture: [_EMPTY_VARIANT] + sorted([
        (str(Label(constraint)), value)
        for (constraint, value) in variants.items()
    ])
    for (architecture, variants) in VARIANT_CONSTRAINTS_BY_ARCHITECTURE.items()
}

def selection_configs(platforms):
    """Return the constraint combinations that select a manifest, and which one they select.

    Args:
        platforms: Ordered list of the OCI platform dicts an image offers, one per manifest.
            An entry may be None for a manifest whose platform is unknown; it is never selected.

    Returns:
        List of `(constraint values, position)` pairs. The constraint combinations are pairwise
        disjoint: two entries always differ in the value of a constraint setting a target platform
        can only hold one value of.
    """
    available = {}
    for platform in platforms:
        if platform:
            available[(platform.get("os"), platform.get("architecture"))] = True

    result = []
    for (os_constraint, os) in _OS:
        for (cpu_constraint, cpu) in _CPU:
            if (os, cpu) not in available:
                continue

            picks = [
                (variant_constraint, match_platform_index(platforms, os, cpu, variant))
                for (variant_constraint, variant) in _VARIANTS.get(cpu, [_EMPTY_VARIANT])
            ]
            distinct = {position: True for (_, position) in picks}

            if len(distinct) == 1:
                # Every variant of this architecture resolves the same way, so the variant
                # constraint does not need to appear in the generated conditions at all. This is
                # the common case - an index with one manifest per architecture and no variants -
                # and it is also what keeps a variant constraint value the tables do not map (a
                # `//img/constraints/ppc64le` one, say) routable rather than incompatible.
                position = picks[0][1]
                if position != None:
                    result.append(([os_constraint, cpu_constraint], position))
                continue

            for (variant_constraint, position) in picks:
                if position != None:
                    result.append(([os_constraint, cpu_constraint, variant_constraint], position))
    return result

_INCOMPATIBLE = str(Label("//img/base:incompatible"))

_CONFIG_SETTING = """
config_setting(
    name = {name},
    constraint_values = [
{values}    ],
)
"""

# The image has no manifest for the target platform, so anything depending on it is skipped
# rather than failing. Aliased instead of referenced directly so that the dependency chain Bazel
# prints for a skipped target names the image that could not be matched.
_NO_MATCHING_PLATFORM = """
alias(
    name = "no_matching_platform",
    actual = {incompatible},
)
"""

_IMAGE_ALIAS = """
alias(
    name = "image",
    actual = {actual},
    visibility = ["//visibility:public"],
)
"""

def _render_config_setting(name, constraint_values):
    return _CONFIG_SETTING.format(
        name = repr(name),
        values = "".join(["        {},\n".format(repr(value)) for value in constraint_values]),
    )

def render_image_alias(platforms, targets):
    """Render the BUILD file section exposing `:image` for one target per platform.

    Args:
        platforms: Ordered list of OCI platform dicts (or None), one per entry of `targets`.
        targets: Ordered list of label strings, one per entry of `platforms`.

    Returns:
        BUILD file text declaring the `config_setting`s, the incompatible fallback and the
        `:image` alias selecting between them.
    """
    if len(platforms) != len(targets):
        fail("expected one target per platform, got {} platforms and {} targets".format(len(platforms), len(targets)))

    declarations = []
    arms = []
    for (i, (constraint_values, position)) in enumerate(selection_configs(platforms)):
        name = "platform_{}".format(i)
        declarations.append(_render_config_setting(name, constraint_values))
        arms.append((":" + name, targets[position]))
    arms.append(("//conditions:default", ":no_matching_platform"))

    return (
        "".join(declarations) +
        _NO_MATCHING_PLATFORM.format(incompatible = repr(_INCOMPATIBLE)) +
        _IMAGE_ALIAS.format(actual = _render_select(arms))
    )

def render_unconditional_image_alias(target):
    """Render a `:image` that is simply the image itself.

    Used for an image that declares no platform at all: there is nothing to select on, and
    nothing to be incompatible with either.

    Args:
        target: Label string the alias points at.

    Returns:
        BUILD file text declaring the `:image` alias.
    """
    return _IMAGE_ALIAS.format(actual = repr(target))

def _render_select(arms):
    return "select({{\n{arms}    }})".format(
        arms = "".join([
            "        {condition}: {target},\n".format(condition = repr(condition), target = repr(target))
            for (condition, target) in arms
        ]),
    )
