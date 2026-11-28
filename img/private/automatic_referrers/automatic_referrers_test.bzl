"""Analysis tests for automatic referrers (with a fake toolchain)."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//img:providers.bzl", "AutomaticReferrersToolchainInfo", "DeployInfo", "ImageSbomInfo", "ReferrersInfo")

_FAKE_TOOLCHAIN = str(Label("//img/private/automatic_referrers:fake_toolchain"))

# --- fake toolchain -----------------------------------------------------------

def _generate(ctx, toolchain, request):
    """Writes a summary of the request instead of an SBOM, once per format."""
    summary = dict(
        kind = request.kind,
        name = request.name,
        marker = toolchain.data.marker,
        stamp = None if request.stamp == None else [request.stamp.stable_status.basename, request.stamp.volatile_status.basename],
    )
    if request.kind == "manifest":
        summary["layers"] = [
            dict(origin = layer.origin, base = layer.base, scan = layer.scan.short_path if layer.scan != None else None)
            for layer in request.layers
        ]
        summary["metadata_graph"] = request.metadata_graph.basename
        summary["metadata_files"] = sorted([f.basename for f in request.metadata_files.to_list()])
        if request.base != None:
            summary["base_image"] = request.base.image.repository if request.base.image != None else None
            summary["base_manifest"] = request.base.image.digest if request.base.image != None else None
            summary["base_sboms"] = [
                dict(format = sbom.format, encoding = sbom.encoding, digest = sbom.digest, scope = sbom.scope)
                for sbom in request.base.sboms
            ]
    else:
        summary["children"] = len(request.manifests)
        summary["child_sboms"] = [sorted(m.sbom.sboms.keys()) if m.sbom != None else None for m in request.manifests]
    artifacts = []
    for sbom_format in toolchain.data.formats:
        out = ctx.actions.declare_file("{}_fake_{}_{}.json".format(ctx.label.name, request.kind, sbom_format))
        ctx.actions.write(out, json.encode(summary))
        artifacts.append(struct(
            file = out,
            media_type = "application/vnd.example.{}+json".format(sbom_format),
            artifact_type = "application/vnd.example.{}".format(sbom_format),
            annotations = {"org.example.generator": "fake"},
            sbom_format = sbom_format,
        ))
    return artifacts

def _scan_layer(ctx, _toolchain, request):
    """Writes a summary of the request instead of scanning the layer."""
    out = ctx.actions.declare_file(request.name + ".fake_scan.json")
    ctx.actions.write(out, json.encode(dict(
        origin = request.origin,
        media_type = request.media_type,
        arguments = request.contents.arguments if request.contents != None else None,
    )))
    return out

def _fake_toolchain_impl(ctx):
    return [platform_common.ToolchainInfo(
        automatic_referrers = AutomaticReferrersToolchainInfo(
            child_referrers = ctx.attr.child_referrers,
            generate = _generate,
            scan_layer = _scan_layer if ctx.attr.scan else None,
            data = struct(marker = ctx.attr.marker, formats = ctx.attr.formats),
        ),
    )]

fake_automatic_referrers_toolchain = rule(
    implementation = _fake_toolchain_impl,
    attrs = {
        "marker": attr.string(default = "fake"),
        "formats": attr.string_list(default = ["cyclonedx", "spdx"]),
        "child_referrers": attr.string(default = "forward"),
        "scan": attr.bool(),
    },
)

# --- helpers ------------------------------------------------------------------

def _written(env, basename):
    for action in analysistest.target_actions(env):
        for output in action.outputs.to_list():
            if output.basename == basename:
                return action.content
    fail("nothing writes " + basename)

def _referrer_actions(env):
    return [action for action in analysistest.target_actions(env) if action.mnemonic == "AutomaticReferrerManifest"]

_WITH_TOOLCHAIN = {"//command_line_option:extra_toolchains": [_FAKE_TOOLCHAIN]}

_WITH_SCANNING_TOOLCHAIN = {"//command_line_option:extra_toolchains": [str(Label("//img/private/automatic_referrers:fake_scanning_toolchain"))]}

_WITH_DROPPING_TOOLCHAIN = {"//command_line_option:extra_toolchains": [str(Label("//img/private/automatic_referrers:fake_dropping_toolchain"))]}

# --- tests ----------------------------------------------------------------------

def _manifest_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    name = target.label.name

    # One SBOM per format of the toolchain.
    info = target[ImageSbomInfo]
    asserts.equals(env, ["cyclonedx", "spdx"], info.sboms.keys())
    asserts.equals(env, [], info.manifests)
    sboms = info.sboms.values()
    asserts.equals(env, "application/vnd.example.spdx", info.sboms["spdx"].artifact_type)
    asserts.equals(env, [sbom.sbom for sbom in sboms], target[OutputGroupInfo].sbom.to_list())

    # Each is a referrer of its own.
    referrers = target[ReferrersInfo].referrers
    asserts.equals(env, [sbom.referrer for sbom in sboms], [referrer.manifest_info for referrer in referrers])
    asserts.equals(env, ["application/vnd.example.cyclonedx", "application/vnd.example.spdx"], [referrer.artifact_type for referrer in referrers])

    # Each SBOM is wrapped into an artifact whose subject is the image.
    actions = _referrer_actions(env)
    asserts.equals(env, 2, len(actions))
    for (action, sbom_format) in zip(actions, ["cyclonedx", "spdx"]):
        argv = action.argv
        subject = argv[argv.index("--subject-descriptor") + 1]
        asserts.true(env, subject.endswith(name + "_descriptor.json"), "subject should be the image's descriptor, got " + subject)
        asserts.equals(env, "application/vnd.example." + sbom_format, argv[argv.index("--artifact-type") + 1])
        asserts.true(env, "--omit-platform" in argv)
        asserts.true(env, "org.example.generator=fake" in argv)

    # The request: the image's name, and the package metadata of its layers.
    summary = json.decode(_written(env, name + "_fake_manifest_cyclonedx.json"))
    asserts.equals(env, "manifest", summary["kind"])
    asserts.equals(env, "//img/private/automatic_referrers:" + name, summary["name"])
    asserts.equals(env, "fake", summary["marker"])
    asserts.equals(env, ctx.attr.stamp or None, summary["stamp"])
    asserts.true(env, len(summary["metadata_files"]) > 0, "the layer's package metadata should be an input")
    graph = json.decode(_written(env, name + "_automatic_referrers.graph.json"))
    asserts.equals(env, "//img/private/automatic_referrers:" + name, graph["root_target"])
    asserts.true(
        env,
        len([node for node in graph["nodes"] if node["metadata_file"]]) > 0,
        "the graph should have a node with package metadata: " + str(graph["nodes"]),
    )
    return analysistest.end(env)

manifest_test = analysistest.make(
    _manifest_test_impl,
    config_settings = _WITH_TOOLCHAIN,
    attrs = {"stamp": attr.string_list(doc = "Expected status files of request.stamp, or empty for None.")},
)

def _none_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    asserts.false(env, ImageSbomInfo in target, "no SBOM expected")
    asserts.false(env, ReferrersInfo in target, "no automatic referrers expected")
    asserts.false(env, hasattr(target[OutputGroupInfo], "sbom"), "no sbom output group expected")
    asserts.equals(env, [], _referrer_actions(env))
    return analysistest.end(env)

# Without a registered toolchain, images are built as before.
none_without_toolchain_test = analysistest.make(_none_test_impl)

# With one, an image can opt out, and an artifact gets none by default.
none_with_toolchain_test = analysistest.make(_none_test_impl, config_settings = _WITH_TOOLCHAIN)

def _index_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[ImageSbomInfo]
    asserts.equals(env, ["cyclonedx", "spdx"], info.sboms.keys())
    asserts.equals(env, 2, len(info.manifests))
    for child in info.manifests:
        asserts.equals(env, ["cyclonedx", "spdx"], child.sboms.keys())

    # The index's own referrers, then those of each child manifest.
    referrers = target[ReferrersInfo].referrers
    asserts.equals(env, 6, len(referrers))
    asserts.equals(env, [sbom.referrer for sbom in info.sboms.values()], [referrer.manifest_info for referrer in referrers[:2]])

    # The index ships its SBOMs and those of its children, in every format.
    asserts.equals(env, 6, len(target[OutputGroupInfo].sbom.to_list()))

    summary = json.decode(_written(env, target.label.name + "_fake_index_cyclonedx.json"))
    asserts.equals(env, 2, summary["children"])
    asserts.equals(env, [["cyclonedx", "spdx"], ["cyclonedx", "spdx"]], summary["child_sboms"])
    return analysistest.end(env)

index_test = analysistest.make(_index_test_impl, config_settings = _WITH_TOOLCHAIN)

def _pulled_base_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    summary = json.decode(_written(env, target.label.name + "_fake_manifest_cyclonedx.json"))
    asserts.equals(env, "example/base", summary["base_image"])
    asserts.equals(env, ctx.attr.base_manifest, summary["base_manifest"])

    # The SBOMs of the selected child come first (documents before attestations,
    # such as the SBOM layers of a BuildKit attestation manifest and of a cosign
    # .att, but not their provenance), then the index's.
    asserts.equals(env, [
        dict(format = "spdx", encoding = "document", digest = ctx.attr.child_sbom, scope = "manifest"),
        dict(format = "spdx", encoding = "in-toto", digest = ctx.attr.buildkit_attestation, scope = "manifest"),
        dict(format = "spdx", encoding = "dsse", digest = ctx.attr.cosign_attestation, scope = "manifest"),
        dict(format = "cyclonedx", encoding = "document", digest = ctx.attr.index_sbom, scope = "index"),
        dict(format = "cyclonedx", encoding = "sigstore-bundle", digest = ctx.attr.index_attestation, scope = "index"),
    ], summary["base_sboms"])
    return analysistest.end(env)

pulled_base_test = analysistest.make(
    _pulled_base_test_impl,
    config_settings = _WITH_TOOLCHAIN,
    attrs = {
        "base_manifest": attr.string(),
        "child_sbom": attr.string(),
        "index_sbom": attr.string(),
        "index_attestation": attr.string(),
        "cosign_attestation": attr.string(),
        "buildkit_attestation": attr.string(),
    },
)

def _push_test_impl(ctx):
    env = analysistest.begin(ctx)
    referrers = analysistest.target_under_test(env)[DeployInfo].referrers
    asserts.equals(env, ctx.attr.expected_referrers, len(referrers))
    return analysistest.end(env)

push_test = analysistest.make(
    _push_test_impl,
    config_settings = _WITH_TOOLCHAIN,
    attrs = {"expected_referrers": attr.int()},
)

def _dropped_children_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    info = target[ImageSbomInfo]

    # The children's SBOMs are still generated (the index's SBOM is built from them)...
    asserts.equals(env, 2, len(info.manifests))
    for child in info.manifests:
        asserts.equals(env, ["cyclonedx", "spdx"], child.sboms.keys())
    summary = json.decode(_written(env, target.label.name + "_fake_index_cyclonedx.json"))
    asserts.equals(env, [["cyclonedx", "spdx"], ["cyclonedx", "spdx"]], summary["child_sboms"])

    # ...but only the index's are published with it.
    referrers = target[ReferrersInfo].referrers
    asserts.equals(env, [sbom.referrer for sbom in info.sboms.values()], [referrer.manifest_info for referrer in referrers])
    asserts.equals(env, [sbom.sbom for sbom in info.sboms.values()], target[OutputGroupInfo].sbom.to_list())
    return analysistest.end(env)

dropped_children_test = analysistest.make(_dropped_children_test_impl, config_settings = _WITH_DROPPING_TOOLCHAIN)

def _forwarded_children_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)

    # An index without referrers of its own never drops its children's.
    asserts.false(env, ImageSbomInfo in target, "the index opted out")
    asserts.equals(env, 4, len(target[ReferrersInfo].referrers))
    asserts.equals(env, 4, len(target[OutputGroupInfo].sbom.to_list()))
    return analysistest.end(env)

forwarded_children_test = analysistest.make(_forwarded_children_test_impl, config_settings = _WITH_DROPPING_TOOLCHAIN)

push_dropping_test = analysistest.make(
    _push_test_impl,
    config_settings = _WITH_DROPPING_TOOLCHAIN,
    attrs = {"expected_referrers": attr.int()},
)

def _layer_scan_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    summary = json.decode(_written(env, target.label.name + "_fake_manifest_cyclonedx.json"))

    # Every layer, the base image's first, is scanned where it comes from: the scan of a layer
    # shared by several images is the same file for all of them.
    asserts.equals(env, [json.decode(layer) for layer in ctx.attr.expected_layers], summary["layers"])

    # The scans are the image's layer_scans output group.
    asserts.equals(
        env,
        sorted([layer["scan"] for layer in summary["layers"]]),
        sorted([f.short_path for f in target[OutputGroupInfo].layer_scans.to_list()]),
    )

    # ...and not by the image itself.
    asserts.equals(env, [], [a for a in analysistest.target_actions(env) for f in a.outputs.to_list() if f.basename.endswith(".fake_scan.json")])
    return analysistest.end(env)

layer_scan_test = analysistest.make(
    _layer_scan_test_impl,
    config_settings = _WITH_SCANNING_TOOLCHAIN,
    attrs = {"expected_layers": attr.string_list(doc = "JSON of each expected request.layers entry.")},
)

def _no_layer_scan_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    summary = json.decode(_written(env, target.label.name + "_fake_manifest_cyclonedx.json"))

    # Without scan_layer, the layers are listed, but not scanned.
    asserts.equals(env, [dict(origin = "built", base = False, scan = None)], summary["layers"])
    return analysistest.end(env)

no_layer_scan_test = analysistest.make(_no_layer_scan_test_impl, config_settings = _WITH_TOOLCHAIN)
