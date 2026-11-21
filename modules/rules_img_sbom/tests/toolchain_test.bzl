"""Analysis tests of sbom_toolchain's child_sboms setting."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("@rules_img//img:providers.bzl", "ImageSbomInfo", "ReferrersInfo")

def _drops_children_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[ImageSbomInfo]

    # The manifests still get their SBOMs: the index's aggregates them.
    asserts.equals(env, 2, len(info.manifests))

    # But only the index's own SBOMs are published with it.
    referrers = target[ReferrersInfo].referrers
    asserts.equals(env, [sbom.referrer for sbom in info.sboms.values()], [r.manifest_info for r in referrers])
    asserts.equals(env, sorted([sbom.sbom.basename for sbom in info.sboms.values()]), sorted([f.basename for f in target[OutputGroupInfo].sbom.to_list()]))

    # And the index's SBOM doesn't refer to SBOMs that aren't published.
    for action in analysistest.target_actions(env):
        for output in action.outputs.to_list():
            if output.basename.endswith(".sbom_request.json"):
                asserts.false(env, json.decode(action.content)["reference_children"])
    return analysistest.end(env)

drops_children_test = analysistest.make(
    _drops_children_test_impl,
    config_settings = {"//command_line_option:extra_toolchains": [str(Label("//tests:dropping_toolchain"))]},
)

def _rejects_dangling_references_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "child_sboms = \"drop\" needs index_sbom = \"aggregate\"")
    return analysistest.end(env)

rejects_dangling_references_test = analysistest.make(_rejects_dangling_references_test_impl, expect_failure = True)
