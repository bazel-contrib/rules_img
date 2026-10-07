"""Custom rules that build on MultipleDeployInfo for testing purposes."""

load("@rules_img//img:providers.bzl", "DeployInfo", "MultipleDeployInfo")

def _deploy_bundle_impl(ctx):
    return [MultipleDeployInfo(
        infos = depset([operation[DeployInfo] for operation in ctx.attr.operations]),
    )]

deploy_bundle = rule(
    implementation = _deploy_bundle_impl,
    doc = "Bundles several operations into a single MultipleDeployInfo.",
    attrs = {
        "operations": attr.label_list(
            mandatory = True,
            providers = [DeployInfo],
            doc = "List of operations to bundle.",
        ),
    },
)

def _no_deploy_infos_impl(_ctx):
    return [MultipleDeployInfo()]

no_deploy_infos = rule(
    implementation = _no_deploy_infos_impl,
    doc = "Provides empty MultipleDeployInfo for testing.",
)
