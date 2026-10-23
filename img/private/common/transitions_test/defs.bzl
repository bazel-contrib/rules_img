"""Analysis tests for image platform transitions: manifests share the configuration of a plain build of their platform, and a nested base index is configured on the manifest's platform by default or once with index_platform = "multiarch"."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//img:image.bzl", "image_index", "image_manifest")
load("//img:layer.bzl", "image_layer")

_LINUX_AMD64 = Label(":linux_amd64")
_OUTER_PLATFORM = Label(":outer_platform")
_PLATFORMS = "//command_line_option:platforms"
_IMAGE_DEPENDENCY_ATTRS = ["manifests", "base"]

_ConfigurationsInfo = provider(
    doc = "Tracks the actual configurations of an index, its manifests and its transitive base index.",
    fields = {
        "base_configurations": "Configurations of the *_base_index target reached along the image dependencies.",
        "configuration": "Configuration of this target.",
        "manifest_configurations": "Configurations of the manifests listed directly in this target's `manifests`.",
        "mismatched_bases": "Manifests whose *_base_index is configured differently from the manifest.",
        "platform": "Target platform of this target.",
    },
)

def _is_base_index(label):
    return label.name.endswith("_base_index")

def _collect_configurations_impl(target, ctx):
    base_configurations = [ctx.configuration] if _is_base_index(target.label) else []
    manifest_configurations = []
    mismatched_bases = []
    base = getattr(ctx.rule.attr, "base", None)
    if ctx.rule.kind == "image_manifest" and base and _is_base_index(base.label):
        if base[_ConfigurationsInfo].configuration != ctx.configuration:
            mismatched_bases.append(target.label)
    for name in _IMAGE_DEPENDENCY_ATTRS:
        dependencies = getattr(ctx.rule.attr, name, [])
        if dependencies == None:
            continue
        if type(dependencies) != "list":
            dependencies = [dependencies]
        for dependency in dependencies:
            info = dependency[_ConfigurationsInfo]
            base_configurations.extend(info.base_configurations)
            mismatched_bases.extend(info.mismatched_bases)
            if name == "manifests":
                manifest_configurations.append(info.configuration)
    return [_ConfigurationsInfo(
        base_configurations = base_configurations,
        configuration = ctx.configuration,
        manifest_configurations = manifest_configurations,
        mismatched_bases = mismatched_bases,
        platform = ctx.fragments.platform.platform,
    )]

_collect_configurations = aspect(
    implementation = _collect_configurations_impl,
    attr_aspects = _IMAGE_DEPENDENCY_ATTRS,
    fragments = ["platform"],
)

# With index_platform = "multiarch", a base index reached through every
# manifest branch has one configuration, the outer index's.
def _multiarch_shares_base_index_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_ConfigurationsInfo]
    configurations = []
    for configuration in info.base_configurations:
        if configuration not in configurations:
            configurations.append(configuration)
    asserts.equals(env, ctx.attr.expected_manifest_branches, len(info.base_configurations), "All manifest branches must reach the base index")
    asserts.equals(env, 1, len(configurations), "The base index must exist in exactly one configuration")
    for configuration in configurations:
        asserts.equals(env, info.configuration, configuration, "The base index must share the outer index's configuration")
    return analysistest.end(env)

_multiarch_shares_base_index_test = analysistest.make(
    _multiarch_shares_base_index_test_impl,
    attrs = {"expected_manifest_branches": attr.int(mandatory = True)},
    config_settings = {_PLATFORMS: [str(_OUTER_PLATFORM)]},
    extra_target_under_test_aspects = [_collect_configurations],
)

# By default, a base index is configured on the platform of each manifest that
# reaches it.
def _default_base_index_on_manifest_platform_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_ConfigurationsInfo]
    asserts.equals(env, ctx.attr.expected_manifest_branches, len(info.base_configurations), "All manifest branches must reach the base index")
    asserts.equals(env, [], info.mismatched_bases, "The base index must be configured on the platform of the manifest that reaches it")
    return analysistest.end(env)

_default_base_index_on_manifest_platform_test = analysistest.make(
    _default_base_index_on_manifest_platform_test_impl,
    attrs = {"expected_manifest_branches": attr.int(mandatory = True)},
    config_settings = {_PLATFORMS: [str(_OUTER_PLATFORM)]},
    extra_target_under_test_aspects = [_collect_configurations],
)

# An index without platforms keeps the platform it is reached from, even with
# index_platform = "multiarch".
def _multiarch_index_without_platforms_keeps_platform_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_ConfigurationsInfo]
    asserts.equals(env, _OUTER_PLATFORM, info.platform, "An index without platforms must keep its incoming platform")
    return analysistest.end(env)

_multiarch_index_without_platforms_keeps_platform_test = analysistest.make(
    _multiarch_index_without_platforms_keeps_platform_test_impl,
    config_settings = {_PLATFORMS: [str(_OUTER_PLATFORM)]},
    extra_target_under_test_aspects = [_collect_configurations],
)

def _plain_linux_amd64_transition_impl(_settings, _attr):
    return {_PLATFORMS: str(_LINUX_AMD64)}

_plain_linux_amd64_transition = transition(
    implementation = _plain_linux_amd64_transition_impl,
    inputs = [],
    outputs = [_PLATFORMS],
)

_ConfigurationInfo = provider(
    doc = "The configuration of one target.",
    fields = {"configuration": "Configuration of the target."},
)

def _record_configuration_impl(_target, ctx):
    return [_ConfigurationInfo(configuration = ctx.configuration)]

_record_configuration = aspect(implementation = _record_configuration_impl)

_PlainComparisonInfo = provider(
    doc = "Configurations to compare: manifests reached through an index, and a plain linux_amd64 build.",
    fields = {
        "manifest_configurations": "Configurations of the manifests listed in the index.",
        "plain_configuration": "Configuration of a target built with only --platforms set to linux_amd64.",
    },
)

def _plain_comparison_impl(ctx):
    plain = ctx.attr.plain
    if type(plain) == "list":
        plain = plain[0]
    return [_PlainComparisonInfo(
        manifest_configurations = ctx.attr.index[_ConfigurationsInfo].manifest_configurations,
        plain_configuration = plain[_ConfigurationInfo].configuration,
    )]

_plain_comparison = rule(
    implementation = _plain_comparison_impl,
    attrs = {
        "index": attr.label(aspects = [_collect_configurations]),
        "plain": attr.label(cfg = _plain_linux_amd64_transition, aspects = [_record_configuration]),
    },
)

# A manifest reached through an index is configured exactly like a plain
# --platforms build of its platform (#808).
def _manifest_matches_plain_build_test_impl(ctx):
    env = analysistest.begin(ctx)
    info = analysistest.target_under_test(env)[_PlainComparisonInfo]
    asserts.equals(env, [info.plain_configuration], info.manifest_configurations, "A manifest must be configured exactly like a plain build of its platform")
    return analysistest.end(env)

_manifest_matches_plain_build_test = analysistest.make(
    _manifest_matches_plain_build_test_impl,
    config_settings = {_PLATFORMS: [str(_OUTER_PLATFORM)]},
)

def transitions_test_suite(name):
    """Creates nested image targets and analysis tests over them; no image actions execute.

    Args:
        name: Name of the wrapping test suite.
    """

    # outer_platform differs from every image platform below, so each split
    # and manifest transition changes the configuration whatever the host is.
    # Its constraints don't matter: no image actions execute.
    native.platform(
        name = "outer_platform",
        constraint_values = ["@platforms//os:osx", "@platforms//cpu:aarch64"],
    )
    native.platform(
        name = "linux_amd64",
        constraint_values = ["@platforms//os:linux", "@platforms//cpu:x86_64"],
    )
    native.platform(
        name = "linux_arm64",
        constraint_values = ["@platforms//os:linux", "@platforms//cpu:aarch64"],
    )

    image_manifest(name = "base_manifest", tags = ["manual"])
    image_layer(
        name = "application_layer",
        srcs = {"app": ":app.txt"},
        tags = ["manual"],
    )

    tests = []
    for mode, index_platform, test_rule, test_suffix in [
        ("default", "inherited", _default_base_index_on_manifest_platform_test, "_base_follows_manifest_test"),
        ("multiarch", "multiarch", _multiarch_shares_base_index_test, "_base_shared_test"),
    ]:
        image_index(
            name = mode + "_base_index",
            manifests = [":base_manifest"],
            platforms = [":linux_amd64", ":linux_arm64"],
            index_platform = index_platform,
            tags = ["manual"],
        )

        # split: the manifest takes each platform of the index's split.
        # same: the manifest's platform already matches the split.
        # override: the manifest forces amd64 under an arm64 split.
        for case, platform, platforms in [
            ("split", None, [":linux_amd64", ":linux_arm64"]),
            ("same", ":linux_amd64", [":linux_amd64"]),
            ("override", ":linux_amd64", [":linux_arm64"]),
        ]:
            prefix = mode + "_" + case
            image_manifest(
                name = prefix + "_manifest",
                base = ":" + mode + "_base_index",
                layers = [":application_layer"],
                platform = platform,
                tags = ["manual"],
            )
            image_index(
                name = prefix + "_index",
                manifests = [":" + prefix + "_manifest"],
                platforms = platforms,
                index_platform = index_platform,
                tags = ["manual"],
            )
            test_rule(
                name = prefix + test_suffix,
                target_under_test = ":" + prefix + "_index",
                expected_manifest_branches = len(platforms),
                size = "small",
            )
            tests.append(":" + prefix + test_suffix)

    image_manifest(
        name = "amd64_manifest",
        platform = ":linux_amd64",
        tags = ["manual"],
    )
    image_index(
        name = "index_without_platforms",
        manifests = [":amd64_manifest"],
        index_platform = "multiarch",
        tags = ["manual"],
    )
    _multiarch_index_without_platforms_keeps_platform_test(
        name = "multiarch_without_platforms_test",
        target_under_test = ":index_without_platforms",
        size = "small",
    )
    tests.append(":multiarch_without_platforms_test")

    _plain_comparison(
        name = "manifest_vs_plain_build",
        index = ":default_same_index",
        plain = ":application_layer",
        tags = ["manual"],
    )
    _manifest_matches_plain_build_test(
        name = "default_matches_plain_build_test",
        target_under_test = ":manifest_vs_plain_build",
        size = "small",
    )
    tests.append(":default_matches_plain_build_test")

    native.test_suite(name = name, tests = tests)
