"""Defines the `signing_config` rule that describes how `img deploy` signs images."""

load("//img/private/providers:signing_config_info.bzl", "SigningConfigInfo")

_SIGN_TARGETS = ["roots", "child_manifests", "referrers"]

def rlocationpath(file, workspace_name):
    """Return the runfiles rlocation path of a file (as accepted by runfiles.Rlocation)."""
    if file.short_path.startswith("../"):
        return file.short_path[len("../"):]
    return workspace_name + "/" + file.short_path

def _signing_config_impl(ctx):
    has_tool = ctx.attr.tool != None
    has_command = ctx.attr.tool_command != ""
    if has_tool == has_command:
        fail("signing_config requires exactly one of 'tool' (a Bazel executable) or 'tool_command' (a host tool name/path)")
    for t in ctx.attr.targets:
        if t not in _SIGN_TARGETS:
            fail("signing_config: invalid target {}; must be one of {}".format(repr(t), _SIGN_TARGETS))

    # Build the config dict in a fixed key order so json.encode is deterministic
    # (its content digest must match on every build and at deploy time).
    config = {"schema_version": 1}
    runfiles = ctx.runfiles()
    if has_tool:
        exe = ctx.executable.tool
        config["mode"] = "rlocation"
        config["tool"] = rlocationpath(exe, ctx.workspace_name)
        runfiles = ctx.runfiles(files = [exe]).merge(ctx.attr.tool[DefaultInfo].default_runfiles)
    else:
        config["mode"] = "command"
        config["tool"] = ctx.attr.tool_command
    if ctx.attr.args:
        config["args"] = ctx.attr.args
    if ctx.attr.env:
        config["env"] = ctx.attr.env

    config_file = ctx.actions.declare_file(ctx.label.name + ".sign_config.json")
    ctx.actions.write(config_file, json.encode(config))

    return [
        SigningConfigInfo(
            config_file = config_file,
            runfiles = runfiles,
            targets = ctx.attr.targets,
        ),
        DefaultInfo(files = depset([config_file]), runfiles = runfiles),
    ]

signing_config = rule(
    implementation = _signing_config_impl,
    doc = """Describes how `img deploy` signs images by invoking a signer plugin.

`img deploy` runs the plugin as `<tool> sign-oci-artifact [args...]`, writes the
JSON descriptor of the artifact to sign to the plugin's stdin, and expects an OCI
image layout tar (the signature artifact) on stdout. The signature is then
pushed to the image's repository as an OCI referrer. `img` itself performs no
cryptography; all signing logic lives in the plugin.

The plugin is referenced either as a Bazel executable (`tool`, shipped in the
push binary's runfiles) or as a host-installed command (`tool_command`, resolved
on `$PATH` at deploy time). Secrets and signing hardware are provided by the
environment `bazel run` executes in — never by Bazel.

Example:

```python
load("@rules_img//img:signing.bzl", "signing_config")

# Use a Bazel-built plugin.
signing_config(
    name = "notary",
    tool = "@rules_img//img/signer:notation",
    args = ["--key", "release"],
)

# Use a host-installed tool.
signing_config(
    name = "corp",
    tool_command = "corp-signer",
    args = ["--profile", "prod"],
)
```

Referenced globally via `--@rules_img//img/settings:sign_setting=//path:notary`,
or per target via the `sign_setting` attribute of `image_push`/`image_push_spec`.
Signing is enabled with the `sign` attribute or the `//img/settings:sign` flag.
""",
    attrs = {
        "tool": attr.label(
            doc = "A Bazel executable implementing the `sign-oci-artifact` protocol. Shipped in the push binary's runfiles. Mutually exclusive with `tool_command`.",
            executable = True,
            cfg = "target",
        ),
        "tool_command": attr.string(
            doc = "Name or path of a host-installed signer tool, resolved on `$PATH` at deploy time. Mutually exclusive with `tool`.",
        ),
        "args": attr.string_list(
            doc = "Arguments passed to the plugin after the `sign-oci-artifact` subcommand.",
        ),
        "env": attr.string_dict(
            doc = "Additional (non-secret) environment variables set for the plugin. Secrets should come from the deploy-time environment instead.",
        ),
        "targets": attr.string_list(
            doc = "Default set of descriptors to sign: any of \"roots\" (the pushed root, the default), \"child_manifests\" (each child of an index), and \"referrers\" (referrer artifacts such as SBOMs). Overridable at deploy time via `--sign_targets`.",
            default = ["roots"],
            allow_empty = False,
        ),
    },
    provides = [SigningConfigInfo],
)

def _unset_signing_config_impl(ctx):
    # Sentinel used as the default of the //img/settings:sign_setting label flag.
    # config_file == None marks "no signing configured".
    return [SigningConfigInfo(config_file = None, runfiles = ctx.runfiles(), targets = [])]

unset_signing_config = rule(
    implementation = _unset_signing_config_impl,
    doc = "Sentinel signing_config meaning 'no signing configured'; the default of //img/settings:sign_setting.",
    provides = [SigningConfigInfo],
)
