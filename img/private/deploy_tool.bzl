"""Rule used to create targets that have DeployToolInfo."""

load("//img/private/providers:deploy_tool_info.bzl", "DeployToolInfo")

def _img_deploy_tool_impl(ctx):
    return [DeployToolInfo(
        img_deploy_exe = ctx.file.img_deploy_exe,
        launcher_template = ctx.file.launcher_template,
    )]

img_deploy_tool = rule(
    implementation = _img_deploy_tool_impl,
    attrs = {
        "img_deploy_exe": attr.label(
            allow_single_file = True,
            mandatory = True,
        ),
        "launcher_template": attr.label(
            mandatory = True,
            allow_single_file = True,
        ),
    },
    doc = "Defines a set of tools used to deploy images.",
    provides = [DeployToolInfo],
)
