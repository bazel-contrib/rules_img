"""Runnable targets that maintain the container images pulled in MODULE.bazel."""

load("@hermetic_launcher//launcher:lib.bzl", "launcher")
load("//img/private/providers:deploy_tool_info.bzl", "DeployToolInfo")

def _pull_command_impl(ctx):
    deploy_tool_info = ctx.attr._deploy_tool[DeployToolInfo]
    stub = ctx.actions.declare_file(ctx.label.name + ".exe")
    embedded_args, transformed_args = launcher.args_from_entrypoint(executable_file = deploy_tool_info.img_deploy_exe)

    # The label tells the command what to show in its help and hints.
    embedded_args.extend([
        "update-images",
        ctx.attr.command,
        "--bazel-target",
        "@rules_img//{}:{}".format(ctx.label.package, ctx.label.name),
    ])
    files = [deploy_tool_info.img_deploy_exe]
    if ctx.file.inventory != None:
        embedded_args.append("--inventory")
        embedded_args, transformed_args = launcher.append_runfile(
            file = ctx.file.inventory,
            embedded_args = embedded_args,
            transformed_args = transformed_args,
        )
        files.append(ctx.file.inventory)
    launcher.compile_stub(
        ctx = ctx,
        embedded_args = embedded_args,
        transformed_args = transformed_args,
        output_file = stub,
        template_file = deploy_tool_info.launcher_template,
    )
    return [DefaultInfo(executable = stub, runfiles = ctx.runfiles(files = files))]

pull_command = rule(
    implementation = _pull_command_impl,
    doc = """Runs a command of `img update-images`, which maintains the images pulled in MODULE.bazel.

Run it with `bazel run`: it reads (and can edit) the files of the workspace it
is run in. See [docs/maintaining-pulled-images.md](/docs/maintaining-pulled-images.md).""",
    attrs = {
        "command": attr.string(
            mandatory = True,
            values = ["list", "pin", "referrers", "update"],
            doc = "The command to run. The target should be named after it.",
        ),
        "inventory": attr.label(
            allow_single_file = [".json"],
            doc = "The inventory.json of the images extension's hub repository, to also look at the images of other modules.",
        ),
        "_deploy_tool": attr.label(
            default = Label("//img/deploy_tool/for_host"),
            providers = [DeployToolInfo],
        ),
    },
    executable = True,
    toolchains = [launcher.finalizer_toolchain_type],
)
