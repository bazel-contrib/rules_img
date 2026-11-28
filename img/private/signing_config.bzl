"""Defines the `signing_config` rule that describes how `img deploy` signs images."""

load("//img/private/providers:signing_config_info.bzl", "SigningConfigInfo")

_SIGN_TARGETS = ["roots", "child_manifests", "referrers", "sbom_attestations"]

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
    doc = """Configures how pushed images are signed: which signer plugin to run, and what to sign.

```python
load("@rules_img//img:signing.bzl", "signing_config")

signing_config(
    name = "sign",
    tool = "@rules_img_signer_cosign",
    targets = ["roots", "child_manifests"],
)
```

Turn signing on with `--@rules_img//img/settings:sign=enabled` and select the config with
`--@rules_img//img/settings:sign_setting=//:sign` (or the `sign` and `sign_setting` attributes of
`image_push` and `image_push_spec`). Keys and tokens come from the environment `bazel run` runs
in, never from Bazel.

See [image signing](/docs/image-signing.md) for how signing works and how to choose a plugin, and
[supply chain security](/docs/supply-chain.md) for signing and attesting SBOMs.
""",
    attrs = {
        "tool": attr.label(
            doc = "The signer plugin, as a Bazel executable, such as `@rules_img_signer_cosign` or `@rules_img_signer_notation`. Mutually exclusive with `tool_command`.",
            executable = True,
            cfg = "target",
        ),
        "tool_command": attr.string(
            doc = "The signer plugin, as the name or path of a command installed on the machine that pushes, resolved on `$PATH` at deploy time. Mutually exclusive with `tool`.",
        ),
        "args": attr.string_list(
            doc = "Arguments for the plugin, such as `--tlog-upload=false` for cosign. See the plugin's README.",
        ),
        "env": attr.string_dict(
            doc = "Additional (non-secret) environment variables set for the plugin. Secrets should come from the deploy-time environment instead.",
        ),
        "targets": attr.string_list(
            doc = """What to sign, any of:

* **`roots`** (default): the pushed image or index.
* **`child_manifests`**: each platform manifest of a pushed index.
* **`referrers`**: the referrers pushed with the image, such as its SBOMs.
* **`sbom_attestations`**: not a signature, but a signed in-toto attestation of each SBOM about
  the image it describes. Needs a plugin that can attest (cosign's, not Notation's).

Overridable at deploy time with `--sign_targets`.""",
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
