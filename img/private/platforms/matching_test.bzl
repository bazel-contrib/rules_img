"""Unit tests for platform spec parsing and index child selection."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//img/private/platforms:matching.bzl", "format_platform", "matching_spec_indices", "parse_platform_spec", "select_index_children")

_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
_INDEX = "application/vnd.oci.image.index.v1+json"
_ARTIFACT = "application/vnd.example.sbom.v1+json"

def _platform(os, architecture, variant = None):
    platform = {"os": os, "architecture": architecture}
    if variant != None:
        platform["variant"] = variant
    return platform

def _child(digest, platform = None, media_type = _MANIFEST):
    child = {"mediaType": media_type, "digest": digest}
    if platform != None:
        child["platform"] = platform
    return child

def _matches(specs, os, architecture, variant = None):
    return len(matching_spec_indices(
        _platform(os, architecture, variant),
        [parse_platform_spec(spec) for spec in specs],
    )) > 0

def _matching_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.true(env, _matches(["linux/amd64"], "linux", "amd64"), "an exact match")
    asserts.false(env, _matches(["linux/amd64"], "linux", "arm64"), "a different architecture")
    asserts.false(env, _matches(["linux/amd64"], "windows", "amd64"), "a different os")

    # Docker Hub lists arm64 with variant v8, which normalizes to the empty variant.
    asserts.true(env, _matches(["linux/arm64"], "linux", "arm64", "v8"), "arm64/v8 is plain arm64")
    asserts.true(env, _matches(["linux/arm64/v8"], "linux", "arm64"), "plain arm64 is arm64/v8")
    asserts.true(env, _matches(["linux/amd64/v1"], "linux", "amd64"), "amd64/v1 is plain amd64")
    asserts.true(env, _matches(["linux/arm/v7"], "linux", "arm"), "arm defaults to v7")
    asserts.true(env, _matches(["linux/aarch64"], "linux", "arm64"), "aarch64 is an alias of arm64")
    asserts.true(env, _matches(["linux/x86_64"], "linux", "amd64"), "x86_64 is an alias of amd64")
    asserts.true(env, _matches(["LINUX/AMD64"], "linux", "amd64"), "specs are case insensitive")

    # A spec that names no variant covers the whole architecture, so filtering cannot take
    # away a child that select_base would otherwise have picked.
    asserts.true(env, _matches(["linux/amd64"], "linux", "amd64", "v3"), "amd64 covers amd64/v3")
    asserts.true(env, _matches(["linux/arm"], "linux", "arm", "v6"), "arm covers arm/v6")
    asserts.true(env, _matches(["linux/arm"], "linux", "arm", "v7"), "arm covers arm/v7")
    asserts.true(env, _matches(["linux/arm64"], "linux", "arm64", "v9"), "arm64 covers arm64/v9")

    # Naming a variant selects just that one.
    asserts.true(env, _matches(["linux/amd64/v3"], "linux", "amd64", "v3"), "an explicit variant matches")
    asserts.false(env, _matches(["linux/amd64/v3"], "linux", "amd64"), "an explicit variant is exact")
    asserts.false(env, _matches(["linux/arm64/v8"], "linux", "arm64", "v9"), "an explicit variant is exact")

    # Unlike containerd's Only(), no compatibility vector is applied across architectures:
    # a filter must not download platforms the caller did not ask for.
    asserts.false(env, _matches(["linux/amd64"], "linux", "386"), "amd64 does not imply 386")

    # Attestation manifests only match when they are requested explicitly.
    asserts.false(env, _matches(["linux/amd64"], "unknown", "unknown"), "attestations are not a platform")
    asserts.true(env, _matches(["unknown/unknown"], "unknown", "unknown"), "attestations can be requested")

    asserts.equals(
        env,
        [],
        matching_spec_indices({}, [parse_platform_spec("linux/amd64")]),
        "an entry without platform information cannot be selected",
    )
    asserts.true(
        env,
        _matches(["windows/amd64", "linux/amd64"], "linux", "amd64"),
        "any of the specs may match",
    )

    return unittest.end(env)

_matching_test = unittest.make(_matching_test_impl)

def _matching_spec_indices_test_impl(ctx):
    env = unittest.begin(ctx)

    specs = [parse_platform_spec(spec) for spec in ["linux/arm64", "linux/arm64/v8", "linux/amd64"]]
    asserts.equals(
        env,
        [0, 1],
        matching_spec_indices(_platform("linux", "arm64", "v8"), specs),
        "every spec an entry matches is reported, so an unused spec can be detected",
    )
    asserts.equals(env, [], matching_spec_indices(_platform("linux", "s390x"), specs), "no match")

    return unittest.end(env)

_matching_spec_indices_test = unittest.make(_matching_spec_indices_test_impl)

def _format_platform_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, "linux/amd64", format_platform(_platform("linux", "amd64")))
    asserts.equals(env, "linux/arm64/v8", format_platform(_platform("linux", "arm64", "v8")))
    asserts.equals(env, "<no platform>", format_platform({}))

    return unittest.end(env)

_format_platform_test = unittest.make(_format_platform_test_impl)

# An index shaped like what buildkit publishes, plus the two child kinds that are not
# image manifests at all.
_AMD64 = _child("sha256:amd64", _platform("linux", "amd64"))
_AMD64_V3 = _child("sha256:amd64v3", _platform("linux", "amd64", "v3"))
_ARM64 = _child("sha256:arm64", _platform("linux", "arm64", "v8"))
_ARM_V6 = _child("sha256:armv6", _platform("linux", "arm", "v6"))
_ARM_V7 = _child("sha256:armv7", _platform("linux", "arm", "v7"))
_ATTESTATION = _child("sha256:attestation", _platform("unknown", "unknown"))
_NO_PLATFORM = _child("sha256:noplatform")
_SBOM = _child("sha256:sbom", _platform("linux", "amd64"), media_type = _ARTIFACT)
_NESTED = _child("sha256:nested", _platform("linux", "s390x"), media_type = _INDEX)

_CHILDREN = [_AMD64, _AMD64_V3, _ARM64, _ARM_V6, _ARM_V7, _ATTESTATION, _NO_PLATFORM]

def _select(children, specs):
    return select_index_children(children, [parse_platform_spec(spec) for spec in specs])

def _digests(children):
    return [child["digest"] for child in children]

def _select_index_children_test_impl(ctx):
    env = unittest.begin(ctx)

    unfiltered = _select(_CHILDREN, [])
    asserts.equals(env, _digests(_CHILDREN), _digests(unfiltered.selected), "no spec selects every child")
    asserts.equals(env, [], unfiltered.omitted, "no spec omits nothing")
    asserts.equals(env, [], unfiltered.unmatched, "no spec can go unmatched")

    one = _select(_CHILDREN, ["linux/arm64"])
    asserts.equals(env, ["sha256:arm64"], _digests(one.selected))
    asserts.equals(
        env,
        ["sha256:amd64", "sha256:amd64v3", "sha256:armv6", "sha256:armv7", "sha256:attestation", "sha256:noplatform"],
        one.omitted,
        "every child that was not selected is reported as omitted",
    )

    # An architecture without a variant covers all of its variants.
    asserts.equals(env, ["sha256:amd64", "sha256:amd64v3"], _digests(_select(_CHILDREN, ["linux/amd64"]).selected))
    asserts.equals(env, ["sha256:armv6", "sha256:armv7"], _digests(_select(_CHILDREN, ["linux/arm"]).selected))
    asserts.equals(env, ["sha256:amd64v3"], _digests(_select(_CHILDREN, ["linux/amd64/v3"]).selected))

    asserts.equals(
        env,
        ["sha256:amd64", "sha256:amd64v3", "sha256:arm64"],
        _digests(_select(_CHILDREN, ["linux/arm64", "linux/amd64", "linux/amd64"]).selected),
        "children are selected once, in index order, however many specs cover them",
    )

    asserts.equals(
        env,
        ["sha256:attestation"],
        _digests(_select(_CHILDREN, ["unknown/unknown"]).selected),
        "attestations are only fetched when asked for by name",
    )

    return unittest.end(env)

_select_index_children_test = unittest.make(_select_index_children_test_impl)

def _select_unmatched_test_impl(ctx):
    env = unittest.begin(ctx)

    missing = _select(_CHILDREN, ["linux/amd64", "linux/ppc64le", "linux/arm64/v9"])
    asserts.equals(
        env,
        ["linux/ppc64le", "linux/arm64/v9"],
        missing.unmatched,
        "specs are reported as written, so the error can quote the attribute",
    )
    asserts.equals(
        env,
        ["linux/amd64", "linux/amd64/v3", "linux/arm64/v8", "linux/arm/v6", "linux/arm/v7", "unknown/unknown", "<no platform>"],
        missing.available,
        "every child's platform is offered as an alternative",
    )
    asserts.equals(
        env,
        ["sha256:amd64", "sha256:amd64v3"],
        _digests(missing.selected),
        "selection is still computed, but the caller fails before downloading it",
    )

    duplicates = _select([_AMD64, _child("sha256:amd64again", _platform("linux", "amd64"))], ["linux/amd64"])
    asserts.equals(env, [], duplicates.unmatched)
    asserts.equals(env, 1, len(duplicates.available), "identical platforms are listed once")

    return unittest.end(env)

_select_unmatched_test = unittest.make(_select_unmatched_test_impl)

def _select_non_image_children_test_impl(ctx):
    env = unittest.begin(ctx)

    children = [_AMD64, _SBOM, _NESTED]

    unfiltered = _select(children, [])
    asserts.equals(env, ["sha256:amd64"], _digests(unfiltered.selected))
    asserts.equals(
        env,
        ["sha256:sbom"],
        unfiltered.omitted,
        "a child that is not an image manifest is never downloaded, so it is omitted",
    )
    asserts.equals(env, ["sha256:nested"], unfiltered.nested, "a nested index is reported to the caller")

    filtered = _select(children, ["linux/amd64"])
    asserts.equals(env, ["sha256:amd64"], _digests(filtered.selected))
    asserts.equals(
        env,
        ["sha256:sbom", "sha256:nested"],
        filtered.omitted,
        "a nested index the filter rejected is omitted instead of reported",
    )
    asserts.equals(env, [], filtered.nested)

    asserts.equals(
        env,
        ["sha256:nested"],
        _select(children, ["linux/amd64", "linux/s390x"]).nested,
        "a nested index the filter selected is still unsupported",
    )

    return unittest.end(env)

_select_non_image_children_test = unittest.make(_select_non_image_children_test_impl)

def matching_test_suite(name):
    """Declare the platform matching unit tests.

    Args:
        name: Name for the test suite.
    """
    unittest.suite(
        name,
        _matching_test,
        _matching_spec_indices_test,
        _format_platform_test,
        _select_index_children_test,
        _select_unmatched_test,
        _select_non_image_children_test,
    )
