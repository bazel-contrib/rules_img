"""Unit tests for the generated per-platform routing conditions."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load(":selection.bzl", "render_image_alias", "render_unconditional_image_alias", "selection_configs")

_LINUX = str(Label("@platforms//os:linux"))
_AMD64 = str(Label("@platforms//cpu:x86_64"))
_ARM64 = str(Label("@platforms//cpu:aarch64"))
_ARM = str(Label("@platforms//cpu:armv7"))
_S390X = str(Label("@platforms//cpu:s390x"))
_EMPTY_VARIANT = str(Label("//img/constraints:empty_variant"))
_V2 = str(Label("//img/constraints/amd64:v2"))
_V3 = str(Label("//img/constraints/amd64:v3"))
_V4 = str(Label("//img/constraints/amd64:v4"))

def _platform(os, architecture, variant = ""):
    return {"os": os, "architecture": architecture, "variant": variant}

def _positions(configs, constraint):
    """Positions selected by every condition that contains a constraint value."""
    return sorted({
        position: True
        for (constraint_values, position) in configs
        if constraint in constraint_values
    }.keys())

def _disjoint_test_impl(ctx):
    env = unittest.begin(ctx)

    configs = selection_configs([
        _platform("linux", "amd64"),
        _platform("linux", "arm64"),
        _platform("linux", "arm", "v6"),
        _platform("linux", "arm", "v7"),
        _platform("windows", "amd64"),
        _platform("unknown", "unknown"),
    ])

    # Two conditions both match a target platform when neither pins a constraint setting to a
    # value the other pins differently - that is, when one constraint set contains the other.
    # Bazel would then either report an ambiguous match or silently resolve to the more
    # specialized arm, so no pair may be in that relation. Distinctness alone is not enough:
    # a collapsed `[os, cpu]` and a per-variant `[os, cpu, variant]` are distinct and both match.
    for (left, _) in configs:
        for (right, _) in configs:
            if left == right:
                continue
            overlap = [value for value in left if value in right]
            asserts.true(
                env,
                len(overlap) < len(left) or len(overlap) < len(right),
                "conditions {} and {} can both match".format(left, right),
            )

    asserts.true(env, len(configs) > 0, "a linux/amd64 index must be selectable somewhere")

    # The aliased spellings of a CPU constraint would generate identical conditions.
    asserts.false(
        env,
        any([str(Label("@platforms//cpu:arm64")) in constraint_values for (constraint_values, _) in configs]),
        "only canonical constraint values are emitted",
    )

    return unittest.end(env)

_disjoint_test = unittest.make(_disjoint_test_impl)

def _collapses_irrelevant_variants_test_impl(ctx):
    env = unittest.begin(ctx)

    configs = selection_configs([_platform("linux", "amd64"), _platform("linux", "arm64")])

    # Every variant of an architecture resolves to the same manifest, so the variant is left out
    # entirely. That also keeps a variant constraint value `tables.bzl` does not map - for
    # instance `//img/constraints/arm64:v8` - matching the condition it belongs to.
    amd64 = [constraint_values for (constraint_values, _) in configs if _AMD64 in constraint_values]
    asserts.equals(env, [[_LINUX, _AMD64]], amd64, "one variant-less condition for amd64")

    arm64 = [constraint_values for (constraint_values, _) in configs if _ARM64 in constraint_values]
    asserts.equals(env, [[_LINUX, _ARM64]], arm64, "one variant-less condition for arm64")

    # The variants of another architecture are not considered at all: they could never match a
    # manifest of this one, and letting them in would defeat the collapse above.
    asserts.false(
        env,
        any([_V3 in constraint_values for (constraint_values, _) in configs]),
        "an amd64 microarchitecture level never appears in an arm64 condition",
    )

    asserts.equals(env, [0], _positions(configs, _AMD64))
    asserts.equals(env, [1], _positions(configs, _ARM64))

    # An architecture that has no variants at all needs no variant constraint either.
    asserts.equals(
        env,
        [([_LINUX, _S390X], 0)],
        selection_configs([_platform("linux", "s390x")]),
        "one variant-less condition for an architecture without variants",
    )

    return unittest.end(env)

_collapses_irrelevant_variants_test = unittest.make(_collapses_irrelevant_variants_test_impl)

def _variant_specific_index_test_impl(ctx):
    env = unittest.begin(ctx)

    # An index whose only amd64 manifest needs v3 must not be reachable from a baseline target.
    configs = selection_configs([_platform("linux", "amd64", "v3")])
    asserts.false(
        env,
        any([_EMPTY_VARIANT in constraint_values for (constraint_values, _) in configs]),
        "a target platform without a microarchitecture level does not select a v3 manifest",
    )
    asserts.false(
        env,
        any([_V2 in constraint_values for (constraint_values, _) in configs]),
        "a v2 target does not select a v3 manifest",
    )
    asserts.equals(env, [0], _positions(configs, _V3))
    asserts.equals(env, [0], _positions(configs, _V4))

    # With both a v2 and a v3 manifest, each target gets the best one it can run.
    configs = selection_configs([_platform("linux", "amd64", "v2"), _platform("linux", "amd64", "v3")])
    asserts.equals(env, [0], _positions(configs, _V2))
    asserts.equals(env, [1], _positions(configs, _V3))
    asserts.equals(env, [1], _positions(configs, _V4))
    asserts.equals(env, [], _positions(configs, _EMPTY_VARIANT))

    return unittest.end(env)

_variant_specific_index_test = unittest.make(_variant_specific_index_test_impl)

def _unroutable_children_test_impl(ctx):
    env = unittest.begin(ctx)

    # Attestation manifests and manifests with no known platform are part of the index, but are
    # never the base image of a build.
    asserts.equals(env, [], selection_configs([_platform("unknown", "unknown")]))
    asserts.equals(env, [], selection_configs([None]))
    asserts.equals(env, [], selection_configs([]))

    # So is an architecture no Bazel constraint value describes. `image_repo` checks for this
    # before constraining a single-platform image, because a condition-less select would make
    # the image incompatible even on the platform it is for.
    asserts.equals(
        env,
        [],
        selection_configs([_platform("linux", "mips64le")]),
        "an unmapped architecture routes nowhere",
    )

    configs = selection_configs([None, _platform("unknown", "unknown"), _platform("linux", "arm", "v7")])
    asserts.equals(env, [2], _positions(configs, _ARM), "only the routable child is ever selected")
    asserts.true(env, len(configs) > 0)
    asserts.true(
        env,
        all([position == 2 for (_, position) in configs]),
        "no condition selects an unroutable child",
    )

    return unittest.end(env)

_unroutable_children_test = unittest.make(_unroutable_children_test_impl)

def _render_test_impl(ctx):
    env = unittest.begin(ctx)

    rendered = render_image_alias([_platform("linux", "amd64")], ["@child//:image"])
    asserts.true(env, 'name = "platform_0"' in rendered, rendered)
    asserts.true(env, repr(_LINUX) in rendered, rendered)
    asserts.true(env, '":platform_0": "@child//:image"' in rendered, rendered)
    asserts.true(env, '"//conditions:default": ":no_matching_platform"' in rendered, rendered)
    asserts.true(env, 'name = "no_matching_platform"' in rendered, rendered)
    asserts.true(env, 'name = "image"' in rendered, rendered)

    # An index with more than one child: every condition has to name the target of the manifest
    # it selects, not the first one. Routing a platform to the wrong child would build against a
    # base for another architecture, and `select_base` accepts the manifest without rechecking it.
    multi = render_image_alias(
        [_platform("linux", "amd64"), _platform("unknown", "unknown"), _platform("linux", "arm64")],
        ["@amd64//:image", "@attestation//:image", "@arm64//:image"],
    )
    asserts.true(env, '"@amd64//:image"' in multi, multi)
    asserts.true(env, '"@arm64//:image"' in multi, multi)
    asserts.false(env, "@attestation//:image" in multi, multi)
    for (condition, target) in [(_AMD64, "@amd64//:image"), (_ARM64, "@arm64//:image")]:
        arms = [
            line
            for line in multi.splitlines()
            if line.strip().startswith('":platform_') and target in line
        ]
        asserts.equals(env, 1, len(arms), "exactly one arm selects {}: {}".format(target, multi))
        for arm in arms:
            name = arm.strip().split('"')[1].removeprefix(":")
            declaration = multi.split('name = "{}"'.format(name))[1].split(")")[0]
            asserts.true(env, condition in declaration, "{} is selected by {}: {}".format(target, condition, multi))

    # Nothing routable: every target platform gets the incompatible fallback.
    unreachable = render_image_alias([None], ["@child//:image"])
    asserts.false(env, "config_setting(" in unreachable, unreachable)
    asserts.true(env, '"//conditions:default": ":no_matching_platform"' in unreachable, unreachable)
    asserts.false(env, "@child//:image" in unreachable, unreachable)

    # An image that declares no platform is not constrained at all.
    unconditional = render_unconditional_image_alias(":original")
    asserts.true(env, 'actual = ":original"' in unconditional, unconditional)
    asserts.false(env, "select(" in unconditional, unconditional)

    return unittest.end(env)

_render_test = unittest.make(_render_test_impl)

def selection_test_suite(name):
    """Declare the routing condition unit tests.

    Args:
        name: Name of the test suite.
    """
    unittest.suite(
        name,
        _disjoint_test,
        _collapses_irrelevant_variants_test,
        _variant_specific_index_test,
        _unroutable_children_test,
        _render_test,
    )
