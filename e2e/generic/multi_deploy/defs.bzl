"""Custom rules that build on multi_deploy_action for testing purposes."""

load(
    "@rules_img//img:multi_deploy.bzl",
    "multi_deploy_action",
    "multi_deploy_action_attrs",
    "multi_deploy_action_toolchains",
)
load("@rules_img//img:providers.bzl", "DeployInfo")

def _split_deploy_impl(ctx):
    operations = [operation[DeployInfo] for operation in ctx.attr.operations]

    # Calling multi_deploy_action more than once from the same rule must yield
    # distinct outputs and non-conflicting runfiles symlinks.
    push_deploy = multi_deploy_action(
        ctx,
        name = ctx.label.name + "_push",
        operations = operations,
        deploy_operations = ["push"],
    )
    load_deploy = multi_deploy_action(
        ctx,
        name = ctx.label.name + "_load",
        operations = operations,
        deploy_operations = ["load"],
    )
    return [
        DefaultInfo(
            files = depset([push_deploy.executable, load_deploy.executable]),
            runfiles = push_deploy.runfiles.merge(load_deploy.runfiles),
        ),
        OutputGroupInfo(
            push_deployer = depset([push_deploy.executable]),
            load_deployer = depset([load_deploy.executable]),
            deploy_manifest = depset([push_deploy.deploy_manifest, load_deploy.deploy_manifest]),
        ),
    ]

split_deploy = rule(
    implementation = _split_deploy_impl,
    doc = "Creates separate push and load deployers for the same operations.",
    attrs = dict(multi_deploy_action_attrs, **{
        "operations": attr.label_list(
            mandatory = True,
            providers = [DeployInfo],
            doc = "List of operations to deploy.",
        ),
    }),
    toolchains = multi_deploy_action_toolchains,
)
