<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for container image signing configuration.

See the `signing_config` rule for how `img deploy` signs images by delegating to
external signer plugins.

<a id="signing_chain"></a>

## signing_chain

<pre>
load("@rules_img//img:signing.bzl", "signing_chain")

signing_chain(<a href="#signing_chain-name">name</a>, <a href="#signing_chain-steps">steps</a>)
</pre>

Runs several signer plugins one after the other, each able to see what the ones before it created.

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

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="signing_chain-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="signing_chain-steps"></a>steps |  The `signing_config` (or `signing_chain`) targets to run, in order. Unlike most label lists, the order is meaningful: later plugins can see what earlier ones created. (buildifier leaves this attribute unsorted.)   | <a href="https://bazel.build/concepts/labels">List of labels</a> | required |  |


<a id="signing_config"></a>

## signing_config

<pre>
load("@rules_img//img:signing.bzl", "signing_config")

signing_config(<a href="#signing_config-name">name</a>, <a href="#signing_config-args">args</a>, <a href="#signing_config-env">env</a>, <a href="#signing_config-protocol">protocol</a>, <a href="#signing_config-targets">targets</a>, <a href="#signing_config-tool">tool</a>, <a href="#signing_config-tool_command">tool_command</a>)
</pre>

Describes how `img deploy` signs images by invoking a signer plugin.

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

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="signing_config-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="signing_config-args"></a>args |  Arguments passed to the plugin after the `sign-oci-artifact` subcommand.   | List of strings | optional |  `[]`  |
| <a id="signing_config-env"></a>env |  Additional (non-secret) environment variables set for the plugin. Secrets should come from the deploy-time environment instead.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="signing_config-protocol"></a>protocol |  What the plugin receives on stdin: `1` (the default) is the JSON descriptor of the artifact, `2` is a JSON request that also carries the destination (registry, repository, tags, full references), the image platform, and deploy facts (`img` version, invocation id, start time). With `2`, the plugin is called once per registry, repository and digest instead of once per push operation. Set it to `2` only for a plugin that documents support for it.   | Integer | optional |  `1`  |
| <a id="signing_config-targets"></a>targets |  Default set of descriptors to process: any of<br><br>- `"roots"` (the pushed root, the default), - `"child_manifests"` (each child of an index), - `"referrers"` (referrer artifacts such as SBOMs), and - `"plugin_artifacts"` (the manifests that plugins *earlier in a `signing_chain`* created, such as a provenance attestation).<br><br>Overridable at deploy time via `--sign_targets`.   | List of strings | optional |  `["roots"]`  |
| <a id="signing_config-tool"></a>tool |  A Bazel executable implementing the `sign-oci-artifact` protocol. Shipped in the push binary's runfiles. Mutually exclusive with `tool_command`.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="signing_config-tool_command"></a>tool_command |  Name or path of a host-installed signer tool, resolved on `$PATH` at deploy time. Mutually exclusive with `tool`.   | String | optional |  `""`  |


