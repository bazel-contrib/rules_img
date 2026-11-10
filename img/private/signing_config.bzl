"""Defines the `signing_config` rule that describes how `img deploy` signs images."""

load("//img/private/providers:signing_config_info.bzl", "SigningConfigInfo")

_SIGN_TARGETS = ["roots", "child_manifests", "referrers", "plugin_artifacts"]

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
    if ctx.attr.protocol != 1:
        # Omitted for the original protocol so existing configs keep their digest.
        config["protocol"] = ctx.attr.protocol
    if ctx.attr.build_info:
        if ctx.attr.protocol != 2:
            fail("signing_config: build_info requires protocol = 2")
        config["build_info"] = True
    if ctx.attr.args:
        config["args"] = ctx.attr.args
    if ctx.attr.env:
        config["env"] = ctx.attr.env

    config_file = ctx.actions.declare_file(ctx.label.name + ".sign_config.json")
    ctx.actions.write(config_file, json.encode(config))

    return [
        SigningConfigInfo(steps = [struct(
            config_file = config_file,
            runfiles = runfiles,
            targets = ctx.attr.targets,
            build_info = ctx.attr.build_info,
        )]),
        DefaultInfo(files = depset([config_file]), runfiles = runfiles),
    ]

signing_config = rule(
    implementation = _signing_config_impl,
    doc = """Describes how `img deploy` signs images by invoking a signer plugin.

`img deploy` runs the plugin as `<tool> sign-oci-artifact [args...]`, writes the
artifact to sign to the plugin's stdin, and expects an OCI image layout tar (the
signature artifact) on stdout. The signature is then pushed to the image's
repository as an OCI referrer. `img` itself performs no cryptography; all signing
logic lives in the plugin.

By default (`protocol = 1`) stdin is the JSON descriptor of the artifact, and the
plugin runs once per push operation and subject. A plugin that needs to know
*where* the image is pushed (its registry, repository and tags), its platform, or
facts about the deploy such as the `img` version opts in with `protocol = 2`:
stdin is then a JSON request holding all of that, and the plugin runs once per
registry, repository and image digest, with the tags of every operation that
pushes it there. Whether to use it is a property of the plugin, so see its
documentation.

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
    tool = "@rules_img_signer_notation",
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
        "protocol": attr.int(
            doc = "What the plugin receives on stdin: `1` (the default) is the JSON descriptor of the artifact, `2` is a JSON request that also carries the destination (registry, repository, tags, full references), the image platform, and deploy facts (`img` version, invocation id, start time). With `2`, the plugin is called once per registry, repository and digest instead of once per push operation. Set it to `2` only for a plugin that documents support for it.",
            default = 1,
            values = [1, 2],
        ),
        "build_info": attr.bool(
            doc = "Give the plugin the Bazel targets the deploy was built from: the label and configuration of the deploy rule, of every image, image index and layer target below it, and the pulled base images, each with its digest. Needs `protocol = 2`; the plugin finds the file in the request. Collecting it costs one small action per deploy target and the digests of the manifests and layer metadata files, so it is off unless the plugin makes use of it.",
            default = False,
        ),
        "args": attr.string_list(
            doc = "Arguments passed to the plugin after the `sign-oci-artifact` subcommand.",
        ),
        "env": attr.string_dict(
            doc = "Additional (non-secret) environment variables set for the plugin. Secrets should come from the deploy-time environment instead.",
        ),
        "targets": attr.string_list(
            doc = """Default set of descriptors to process: any of

- `"roots"` (the pushed root, the default),
- `"child_manifests"` (each child of an index),
- `"referrers"` (referrer artifacts such as SBOMs), and
- `"plugin_artifacts"` (the manifests that plugins *earlier in a `signing_chain`* created, such as a provenance attestation).

Overridable at deploy time via `--sign_targets`.""",
            default = ["roots"],
            allow_empty = False,
        ),
    },
    provides = [SigningConfigInfo],
)

def _signing_chain_impl(ctx):
    steps = []
    seen = {}
    runfiles = ctx.runfiles()
    for config in ctx.attr.steps:
        for step in config[SigningConfigInfo].steps:
            path = step.config_file.path
            if path in seen:
                fail("signing_chain {}: {} is listed more than once; a plugin config can run only once per chain".format(ctx.label, config.label))
            seen[path] = True
            steps.append(step)
            runfiles = runfiles.merge(step.runfiles)
    if not steps:
        fail("signing_chain {} has no plugins".format(ctx.label))
    return [
        SigningConfigInfo(steps = steps),
        DefaultInfo(files = depset([step.config_file for step in steps]), runfiles = runfiles),
    ]

signing_chain = rule(
    implementation = _signing_chain_impl,
    doc = """Runs several signer plugins one after the other, each able to see what the ones before it created.

Use it to sign an image, its SBOM *and* a provenance attestation, which is created at deploy time by
another plugin:

```python
load("@rules_img//img:signing.bzl", "signing_chain", "signing_config")

signing_config(
    name = "provenance",
    tool = "@rules_img_signer_provenance",
    protocol = 2,
    targets = ["roots", "child_manifests"],
)

signing_config(
    name = "cosign",
    tool = "@rules_img_signer_cosign",
    # Sign the image and its SBOM, and also what the plugins before this one made.
    targets = ["roots", "child_manifests", "referrers", "plugin_artifacts"],
)

signing_chain(
    name = "release",
    steps = [":provenance", ":cosign"],  # in this order
)
```

Select the chain like a `signing_config`: `--@rules_img//img/settings:sign_setting=//path:release` or
the `sign_setting` attribute of `image_push`.

The plugins run in the order of `steps`. A plugin sees the manifests that the plugins *before* it
created (an attestation, a signature) only if its `targets` include `"plugin_artifacts"`, and never its
own output or that of the plugins after it. A chain therefore cannot recurse, and each manifest is pushed
before the next plugin attaches something to it. A plugin that does not select `"plugin_artifacts"` just
processes the image as usual.

`targets = ["referrers"]` applies per plugin too: a plugin only processes the SBOM (and other referrers
built into the image) if it selects them, and the plugins after it can then process what it created for
the SBOM as well.

A `signing_chain` can list other chains. The same `signing_config` cannot appear twice.
""",
    attrs = {
        "steps": attr.label_list(
            doc = "The `signing_config` (or `signing_chain`) targets to run, in order. Unlike most label lists, the order is meaningful: later plugins can see what earlier ones created. (buildifier leaves this attribute unsorted.)",
            providers = [SigningConfigInfo],
            allow_empty = False,
            mandatory = True,
        ),
    },
    provides = [SigningConfigInfo],
)

def _unset_signing_config_impl(_ctx):
    # Sentinel used as the default of the //img/settings:sign_setting label flag.
    # No steps marks "no signing configured".
    return [SigningConfigInfo(steps = [])]

unset_signing_config = rule(
    implementation = _unset_signing_config_impl,
    doc = "Sentinel signing_config meaning 'no signing configured'; the default of //img/settings:sign_setting.",
    provides = [SigningConfigInfo],
)
