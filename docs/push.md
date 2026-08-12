<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for container image push rules.

<a id="image_push"></a>

## image_push

<pre>
load("@rules_img//img:push.bzl", "image_push")

image_push(<a href="#image_push-name">name</a>, <a href="#image_push-build_settings">build_settings</a>, <a href="#image_push-cross_mount_from">cross_mount_from</a>, <a href="#image_push-deduplicated_push">deduplicated_push</a>,
           <a href="#image_push-deduplicated_push_blob_repository">deduplicated_push_blob_repository</a>, <a href="#image_push-deduplicated_push_content">deduplicated_push_content</a>, <a href="#image_push-deploy_tool">deploy_tool</a>,
           <a href="#image_push-destination_file">destination_file</a>, <a href="#image_push-forbid_layer_push">forbid_layer_push</a>, <a href="#image_push-image">image</a>, <a href="#image_push-manifest_tags">manifest_tags</a>, <a href="#image_push-push_at_build_time">push_at_build_time</a>,
           <a href="#image_push-push_at_build_time_blob_repository">push_at_build_time_blob_repository</a>, <a href="#image_push-push_at_build_time_content">push_at_build_time_content</a>,
           <a href="#image_push-push_at_build_time_exec_properties">push_at_build_time_exec_properties</a>, <a href="#image_push-push_at_build_time_manifest_repository">push_at_build_time_manifest_repository</a>, <a href="#image_push-referrers">referrers</a>,
           <a href="#image_push-registry">registry</a>, <a href="#image_push-repository">repository</a>, <a href="#image_push-sign">sign</a>, <a href="#image_push-sign_setting">sign_setting</a>, <a href="#image_push-stamp">stamp</a>, <a href="#image_push-strategy">strategy</a>, <a href="#image_push-tag">tag</a>, <a href="#image_push-tag_file">tag_file</a>, <a href="#image_push-tag_list">tag_list</a>,
           <a href="#image_push-tool_cfg">tool_cfg</a>, <a href="#image_push-tracks_content">tracks_content</a>)
</pre>

Pushes container images to a registry.

This rule creates an executable target that uploads OCI images to container registries.
It supports multiple push strategies optimized for different use cases, from simple
uploads to advanced content-addressable storage integration.

Key features:
- **Multiple push strategies**: Choose between eager, lazy, CAS-based, or BES-integrated pushing
- **Template expansion**: Dynamic registry, repository, and tag values using build settings
- **Stamping support**: Include build information in image tags
- **Incremental uploads**: Skip blobs that already exist in the registry

The rule produces an executable that can be run with `bazel run`.

Example:

```python
load("@rules_img//img:push.bzl", "image_push")

# Simple push to Docker Hub
image_push(
    name = "push_app",
    image = ":my_app",
    registry = "index.docker.io",
    repository = "myorg/myapp",
    tag = "latest",
)

# Push multi-platform image with multiple tags
image_push(
    name = "push_multiarch",
    image = ":my_app_index",  # References an image_index
    registry = "gcr.io",
    repository = "my-project/my-app",
    tag_list = ["latest", "v1.0.0"],
)

# Dynamic push with build settings
image_push(
    name = "push_dynamic",
    image = ":my_app",
    registry = "{{.REGISTRY}}",
    repository = "{{.PROJECT}}/my-app",
    tag = "{{.VERSION}}",
    build_settings = {
        "REGISTRY": "//settings:registry",
        "PROJECT": "//settings:project",
        "VERSION": "//settings:version",
    },
)

# Push with stamping for unique tags
image_push(
    name = "push_stamped",
    image = ":my_app",
    registry = "index.docker.io",
    repository = "myorg/myapp",
    tag = "latest-{{.BUILD_TIMESTAMP}}",
    stamp = "force",
)

# Digest-only push (no tag)
image_push(
    name = "push_by_digest",
    image = ":my_app",
    registry = "gcr.io",
    repository = "my-project/my-app",
    # No tag specified - will push by digest only
)

# Push using a destination file (instead of registry/repository attributes)
image_push(
    name = "push_from_file",
    image = ":my_app",
    destination_file = ":push_destination.txt",
    tag = "latest",
)
```

Push strategies:
- **`eager`**: Materializes all layers next to push binary. Simple, correct, but may be inefficient.
- **`lazy`**: Layers are not stored locally. Missing layers are streamed from Bazel's remote cache.
- **`cas_registry`**: Uses content-addressable storage for extreme efficiency. Requires
  CAS-enabled infrastructure.
- **`bes`**: Image is pushed as side-effect of Build Event Stream upload. No "bazel run" command needed.
  Requires Build Event Service integration.

See [push strategies documentation](/docs/push-strategies.md) for detailed comparisons.

Push at build time:

When the `push_at_build_time` setting is enabled, an `image_push` target also
uploads its image content to the registry *during the build* (as `PushImage`
validation actions), so the image is present without a separate `bazel run`. See
[push at build time](/docs/push-strategies.md#push-at-build-time).

Runtime usage:
```bash
# Push to registry
bazel run //path/to:push_app

# The push command will output the image digest
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_push-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_push-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to string_flag targets. These values can be used in registry, repository, and tag attributes using `{{.VARIABLE_NAME}}` syntax (Go template).<br><br>Example: <pre><code class="language-python">build_settings = {&#10;    "REGISTRY": "//settings:docker_registry",&#10;    "VERSION": "//settings:app_version",&#10;}</code></pre><br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="image_push-cross_mount_from"></a>cross_mount_from |  An image_push target whose layers may be cross-mounted during push.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_push-deduplicated_push"></a>deduplicated_push |  Whether `img deploy` uploads a blob several repositories need only once.<br><br>When `enabled`, the deploy first asks the registry which manifests it already holds, then uploads each remaining layer that more than one destination repository needs to just one of them — the first alphabetically — and cross-mounts it into the others. Meant for registries that keep a separate blob store per repository name, where pushing several images that share their layers otherwise uploads every shared layer once per repository. See [deduplicated push](/docs/push-strategies.md#deduplicated-push).<br><br>**`enabled` only works on a registry that supports cross-repository blob mounting.** A push that opted in fails loudly where mounting is refused, rather than silently uploading the blob into every repository. See the [registry support matrix](/docs/registry-support.md).<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:deduplicated_push` flag. - **`enabled`**: deduplicate blob uploads, and fail the push if a layer this deploy   uploaded to a home repository cannot be cross-mounted from there. - **`best_effort`**: deduplicate blob uploads, but upload a layer's bytes the ordinary   way if the registry refuses to mount it (and treat a failed upload to a home   repository as a warning). The deduplication is attempted without the deploy   depending on it, which is what makes it usable on a registry you have not probed. - **`disabled`**: push each manifest independently.<br><br>The setting travels with this push operation, so a deploy that merges several of them (`multi_deploy`, or an image with several `push_specs`) can mix the two: the operations that opted in are planned together and cross-mount between each other, while the rest are pushed exactly as they would be with the setting off.   | String | optional |  `"auto"`  |
| <a id="image_push-deduplicated_push_blob_repository"></a>deduplicated_push_blob_repository |  Repository the deduplicated push uploads this target's shared blobs to.<br><br>When non-empty, every blob the deduplicated push shares between repositories is uploaded to this repository (within the destination registry) and cross-mounted from there. It takes precedence over every home repository the deploy would have picked itself — including a repository the registry already serves the blob from — so that shared blobs all end up in one place: one repository to retain and clean up, and one repository a credential has to be able to read for the mounts to work. The cost is that blobs which were mountable for free are uploaded there.<br><br>Left at its sentinel default, this defers to the global `--@rules_img//img/settings:deduplicated_push_blob_repository` flag. Set it to a string to override per target, or to `""` to force "no pinned repository" even when the global flag is set.<br><br>Only consulted when deduplicated push is active (see `deduplicated_push`).   | String | optional |  `"<use global setting>"`  |
| <a id="image_push-deduplicated_push_content"></a>deduplicated_push_content |  What the deduplicated push writes to a shared blob's home repository.<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:deduplicated_push_content` flag. - **`blobs`**: upload the shared blob to its home repository and nothing else. - **`blobs_and_artificial_manifests`**: additionally upload a config blob and create a   manifest referencing the blob and that config in the home repository, before the blob   counts as available to the repositories that share it. For registries that only expose   a blob to other repositories once a manifest references it: with the manifest in place   the blob is served to every repository the caller may read, so the manifest push finds   it there and uploads nothing — with or without a cross-mount.<br><br>The artificial manifest is a real single-layer image manifest (the layer descriptor is the blob's own and the config records the layer's diff id), pushed by digest and untagged. Note that a registry policy which deletes untagged manifests can undo the blob's visibility along with it.<br><br>Only consulted when deduplicated push is active (see `deduplicated_push`).   | String | optional |  `"auto"`  |
| <a id="image_push-deploy_tool"></a>deploy_tool |  Optional label of a deploy tool target providing `DeployToolInfo` (created with `img_deploy_tool` from `@rules_img//img:deploy_tool.bzl`). When set, overrides `tool_cfg`.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_push-destination_file"></a>destination_file |  File containing the push destination as `{registry}/{repository}`.<br><br>The file should contain a single line with the registry and repository separated by the first `/`. For example: `gcr.io/my-project/my-app`.<br><br>The content is read as a literal string without Go template expansion. Trailing newlines and whitespace are stripped.<br><br>Cannot be used together with `registry` or `repository` attributes.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_push-forbid_layer_push"></a>forbid_layer_push |  Whether `img deploy` is forbidden from uploading layer blob bytes.<br><br>When `enabled`, a `bazel run` deploy of this push may only cross-mount layers server-side or skip layers already present; an actual layer upload fails loudly. Use it together with push at build time (which uploads the layer blobs) so a deploy that would re-upload them is caught instead of silently succeeding.<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:forbid_layer_push` flag. - **`enabled`**: forbid layer blob uploads. - **`disabled`**: allow layer blob uploads.   | String | optional |  `"auto"`  |
| <a id="image_push-image"></a>image |  Image to push. Should provide ImageManifestInfo or ImageIndexInfo.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_push-manifest_tags"></a>manifest_tags |  Per-platform tag templates for multi-platform (`image_index`) pushes.<br><br>Only valid when `image` provides `ImageIndexInfo`. For each entry in this list, the deploy command produces one tag per child manifest in the index by expanding the entry against the platform descriptor of that manifest.<br><br>Available template variables (lowercase):<br><br>- `{{.os}}` — platform OS (e.g. `linux`) - `{{.architecture}}`, `{{.arch}}`, `{{.cpu}}` — architecture (e.g. `amd64`, `arm64`) - `{{.variant}}` — architecture variant (e.g. `v8`), if set<br><br>The tags in `tag` / `tag_list` / `tag_file` continue to point at the index as a whole; `manifest_tags` complement those by publishing additional tags that each resolve to a single child manifest.<br><br>Example:<br><br><pre><code class="language-python">image_push(&#10;    name = "push_multiarch",&#10;    image = ":my_app_index",&#10;    registry = "gcr.io",&#10;    repository = "my-project/my-app",&#10;    tag_list = ["latest", "v1.0.0"],&#10;    manifest_tags = [&#10;        "latest-{{.os}}-{{.architecture}}",&#10;        "v1.0.0-{{.os}}-{{.architecture}}",&#10;    ],&#10;)</code></pre><br><br>Templates are expanded at build time per child manifest, so `build_settings` and stamping variables are available (and override any platform variable of the same name). The expanded tags are emitted as `registry_tag` operations in the deploy manifest, so non-CLI strategies like `bes` can honor them.   | List of strings | optional |  `[]`  |
| <a id="image_push-push_at_build_time"></a>push_at_build_time |  Whether image content is pushed to the registry *during the build*.<br><br>Push at build time wires extra `PushImage` build actions (one per blob, plus a manifest push in `blobs_and_manifests` mode) that upload directly to the registry as a Bazel validation action. See [push at build time](/docs/push-strategies.md#push-at-build-time).<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:push_at_build_time` flag. - **`enabled`**: always push at build time; a push failure fails the build. - **`best_effort`**: push at build time, but a push failure is a warning and does not fail the build. - **`disabled`**: never push at build time.<br><br>When `disabled`, blob cross-mounting via `push_at_build_time_blob_repository` is also not recorded in the deploy manifest, so a later `bazel run` deploy does not try to cross-mount from a staging repository nothing was pushed to.   | String | optional |  `"auto"`  |
| <a id="image_push-push_at_build_time_blob_repository"></a>push_at_build_time_blob_repository |  Staging repository for build-time blob uploads and cross-mounting.<br><br>When non-empty, every image blob (all layers and the config) is pushed to this repository (a "staging" repository within the destination registry) and cross-mounted from there when the manifest is pushed to its real repository.<br><br>Left at its sentinel default, this defers to the global `--@rules_img//img/settings:push_at_build_time_blob_repository` flag. Set it to a string to override per target, or to `""` to force "no staging repository" even when the global flag is set.<br><br>Only takes effect when push at build time is active (see `push_at_build_time`): if push at build time is `disabled` for this target, no cross-mount source is recorded in the deploy manifest even when this is set.   | String | optional |  `"<use global setting>"`  |
| <a id="image_push-push_at_build_time_content"></a>push_at_build_time_content |  What the push-at-build-time actions upload.<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:push_at_build_time_content` flag. - **`blobs`**: push only the layer blobs and the config blob. Manifests/tags are   written afterwards by `image_push` / `multi_deploy`. - **`blobs_and_manifests`**: push the blobs plus the config and manifest(s)/tags,   so the image is fully present in the registry when the build finishes.<br><br>Only consulted when push at build time is active (see `push_at_build_time`).   | String | optional |  `"auto"`  |
| <a id="image_push-push_at_build_time_exec_properties"></a>push_at_build_time_exec_properties |  Execution properties for the `PushImage` build-time push actions.<br><br>Forwarded verbatim as the `execution_requirements` of every `PushImage` action emitted for this target. Defaults to `{"requires-network": "1"}` because the push actions talk to the registry (and are therefore non-hermetic). Override it to add or replace execution properties, for example to route the actions to a specific remote execution pool. Setting it to `{}` removes the `requires-network` marker.<br><br>Only consulted when push at build time is active (see `push_at_build_time`).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{"requires-network": "1"}`  |
| <a id="image_push-push_at_build_time_manifest_repository"></a>push_at_build_time_manifest_repository |  Repository the build-time manifest push writes manifest(s)/config to.<br><br>When non-empty and `push_at_build_time_content` is `blobs_and_manifests`, the build-time manifest push uploads the manifest(s)/index (and, directly, the config) to this repository instead of the image's real repository. This only redirects where manifests are written at build time; it does **not** change where layer blobs are cross-mounted from (that is `push_at_build_time_blob_repository`).<br><br>Left at its sentinel default, this defers to the global `--@rules_img//img/settings:push_at_build_time_manifest_repository` flag. Set it to a string to override per target, or to `""` to force the image's real repository even when the global flag is set.   | String | optional |  `"<use global setting>"`  |
| <a id="image_push-referrers"></a>referrers |  Additional manifests or indexes to push as referrers to the main image.<br><br>Each referrer is pushed to the same registry and repository as the main image, but without tags (referrers are discovered via the OCI referrers API by digest).<br><br>Each target must provide ImageManifestInfo or ImageIndexInfo and must have its `subject` field set to reference the main image being pushed.<br><br>Example: <pre><code class="language-python">image_push(&#10;    name = "push",&#10;    image = ":my_app",&#10;    referrers = [&#10;        ":sbom_manifest",&#10;        ":signature_manifest",&#10;    ],&#10;    registry = "ghcr.io",&#10;    repository = "myorg/myapp",&#10;    tag = "latest",&#10;)</code></pre>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_push-registry"></a>registry |  Registry URL to push the image to.<br><br>Common registries: - Docker Hub: `index.docker.io` - Google Container Registry: `gcr.io` or `us.gcr.io` - GitHub Container Registry: `ghcr.io` - Amazon ECR: `123456789.dkr.ecr.us-east-1.amazonaws.com`<br><br>Subject to [template expansion](/docs/templating.md).   | String | optional |  `""`  |
| <a id="image_push-repository"></a>repository |  Repository path within the registry.<br><br>Subject to [template expansion](/docs/templating.md).   | String | optional |  `""`  |
| <a id="image_push-sign"></a>sign |  Whether `img deploy` signs this image after pushing.<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:sign` flag. - **`enabled`**: always sign; a signing failure (or missing `sign_setting`) fails the deploy. - **`best_effort`**: sign if possible; failures are warnings and do not fail the deploy. - **`disabled`**: never sign.<br><br>The signing plugin is selected by `sign_setting` (or the global `//img/settings:sign_setting`). Signatures are attached as OCI referrers.   | String | optional |  `"auto"`  |
| <a id="image_push-sign_setting"></a>sign_setting |  A `signing_config` target selecting how this image is signed.<br><br>Overrides the global `--@rules_img//img/settings:sign_setting`. Only consulted when signing is enabled (see `sign`).   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_push-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |
| <a id="image_push-strategy"></a>strategy |  Push strategy to use.<br><br>See [push strategies documentation](/docs/push-strategies.md) for detailed information.   | String | optional |  `"auto"`  |
| <a id="image_push-tag"></a>tag |  Tag to apply to the pushed image.<br><br>Optional - if omitted, the image is pushed by digest only.<br><br>Subject to [template expansion](/docs/templating.md).   | String | optional |  `""`  |
| <a id="image_push-tag_file"></a>tag_file |  File containing newline-delimited tags to apply to the pushed image.<br><br>The file should contain one tag per line. Empty lines are ignored. Tags from this file are merged with tags specified via `tag` or `tag_list` attributes.<br><br>Example file content: <pre><code>latest&#10;v1.0.0&#10;stable</code></pre><br><br>Can be combined with `tag` or `tag_list` to merge tags from multiple sources. Each tag is subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_push-tag_list"></a>tag_list |  List of tags to apply to the pushed image.<br><br>Useful for applying multiple tags in a single push:<br><br><pre><code class="language-python">tag_list = ["latest", "v1.0.0", "stable"]</code></pre><br><br>Cannot be used together with `tag`. Can be combined with `tag_file` to merge tags from both sources. Each tag is subject to [template expansion](/docs/templating.md).   | List of strings | optional |  `[]`  |
| <a id="image_push-tool_cfg"></a>tool_cfg |  Configuration of the pusher executable platform.<br><br>Available options: - **`host`** (default): Pusher executable matches the host platform. - **`target`**: Pusher executable matches the target platform(s) specified via `--platforms`.   | String | optional |  `"host"`  |
| <a id="image_push-tracks_content"></a>tracks_content |  When True, the template expansion action depends on the image digest.<br><br>A template string built from a volatile stamp value (e.g. `{{.BUILD_TIMESTAMP}}`) normally freezes on the first build, because Bazel excludes the volatile workspace-status file from the action cache key. With this enabled, the image descriptor becomes an input to the tag-expansion action, so the tag re-stamps whenever the image content (digest) changes, while unchanged content keeps the cached tag.<br><br>The digest is exposed to the `registry`, `repository`, and `tag` templates as `{{.digest}}`. Referencing the digest in the tag is optional: the re-stamp behavior applies whether or not the tag contains it.   | Boolean | optional |  `False`  |


<a id="image_push_spec"></a>

## image_push_spec

<pre>
load("@rules_img//img:push.bzl", "image_push_spec")

image_push_spec(<a href="#image_push_spec-name">name</a>, <a href="#image_push_spec-build_settings">build_settings</a>, <a href="#image_push_spec-cross_mount_from">cross_mount_from</a>, <a href="#image_push_spec-deduplicated_push">deduplicated_push</a>,
                <a href="#image_push_spec-deduplicated_push_blob_repository">deduplicated_push_blob_repository</a>, <a href="#image_push_spec-deduplicated_push_content">deduplicated_push_content</a>, <a href="#image_push_spec-destination_file">destination_file</a>,
                <a href="#image_push_spec-forbid_layer_push">forbid_layer_push</a>, <a href="#image_push_spec-manifest_tags">manifest_tags</a>, <a href="#image_push_spec-push_at_build_time">push_at_build_time</a>,
                <a href="#image_push_spec-push_at_build_time_blob_repository">push_at_build_time_blob_repository</a>, <a href="#image_push_spec-push_at_build_time_content">push_at_build_time_content</a>,
                <a href="#image_push_spec-push_at_build_time_exec_properties">push_at_build_time_exec_properties</a>, <a href="#image_push_spec-push_at_build_time_manifest_repository">push_at_build_time_manifest_repository</a>, <a href="#image_push_spec-referrers">referrers</a>,
                <a href="#image_push_spec-registry">registry</a>, <a href="#image_push_spec-repository">repository</a>, <a href="#image_push_spec-sign">sign</a>, <a href="#image_push_spec-sign_setting">sign_setting</a>, <a href="#image_push_spec-stamp">stamp</a>, <a href="#image_push_spec-strategy">strategy</a>, <a href="#image_push_spec-tag">tag</a>, <a href="#image_push_spec-tag_file">tag_file</a>, <a href="#image_push_spec-tag_list">tag_list</a>,
                <a href="#image_push_spec-tracks_content">tracks_content</a>)
</pre>

Defines push configuration for container images without referencing a specific image.

This rule captures registry, repository, tag, and strategy settings that can be
attached to `image_manifest` or `image_index` targets via their `push_specs`
attribute. Template strings using Go template syntax (`{{.VAR}}`) are accepted
but not expanded — expansion happens when the deployment is consumed by the
image rule.
Note that the template strings `{{.image_target_package}}` and `{{.image_target_name}}` are especially useful here.

This enables an inverted dependency pattern: instead of `image_push` depending
on the image, the image itself carries its deployment configuration, making it
directly usable with `multi_deploy`.

Example:

```python
load("@rules_img//img:push.bzl", "image_push_spec")

image_push_spec(
    name = "push_config",
    registry = "gcr.io",
    repository = "my-project/{{.image_target_package}}/{{.image_target_name}}",
    tag = "{{.VERSION}}",
    build_settings = {
        "VERSION": "//settings:version",
    },
    stamp = "force",
)

# Attach to an image:
image_manifest(
    name = "my_app_a",
    base = "@distroless_cc",
    layers = [":app_layer"],
    push_specs = [":push_config"],
)

# Attach to another image:
image_manifest(
    name = "my_app_b",
    base = "@distroless_cc",
    layers = [":app_layer"],
    push_specs = [":push_config"],
)

# Now usable directly in multi_deploy:
multi_deploy(
    name = "deploy",
    operations = [
        ":my_app_a",
        ":my_app_b",
    ],
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_push_spec-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_push_spec-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to string_flag targets. These values can be used in registry, repository, and tag attributes using `{{.VARIABLE_NAME}}` syntax (Go template).<br><br>Example: <pre><code class="language-python">build_settings = {&#10;    "REGISTRY": "//settings:docker_registry",&#10;    "VERSION": "//settings:app_version",&#10;}</code></pre><br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="image_push_spec-cross_mount_from"></a>cross_mount_from |  An image_push target whose layers may be cross-mounted during push.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_push_spec-deduplicated_push"></a>deduplicated_push |  Whether `img deploy` uploads a blob several repositories need only once.<br><br>When `enabled`, the deploy first asks the registry which manifests it already holds, then uploads each remaining layer that more than one destination repository needs to just one of them — the first alphabetically — and cross-mounts it into the others. Meant for registries that keep a separate blob store per repository name, where pushing several images that share their layers otherwise uploads every shared layer once per repository. See [deduplicated push](/docs/push-strategies.md#deduplicated-push).<br><br>**`enabled` only works on a registry that supports cross-repository blob mounting.** A push that opted in fails loudly where mounting is refused, rather than silently uploading the blob into every repository. See the [registry support matrix](/docs/registry-support.md).<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:deduplicated_push` flag. - **`enabled`**: deduplicate blob uploads, and fail the push if a layer this deploy   uploaded to a home repository cannot be cross-mounted from there. - **`best_effort`**: deduplicate blob uploads, but upload a layer's bytes the ordinary   way if the registry refuses to mount it (and treat a failed upload to a home   repository as a warning). The deduplication is attempted without the deploy   depending on it, which is what makes it usable on a registry you have not probed. - **`disabled`**: push each manifest independently.<br><br>The setting travels with this push operation, so a deploy that merges several of them (`multi_deploy`, or an image with several `push_specs`) can mix the two: the operations that opted in are planned together and cross-mount between each other, while the rest are pushed exactly as they would be with the setting off.   | String | optional |  `"auto"`  |
| <a id="image_push_spec-deduplicated_push_blob_repository"></a>deduplicated_push_blob_repository |  Repository the deduplicated push uploads this target's shared blobs to.<br><br>When non-empty, every blob the deduplicated push shares between repositories is uploaded to this repository (within the destination registry) and cross-mounted from there. It takes precedence over every home repository the deploy would have picked itself — including a repository the registry already serves the blob from — so that shared blobs all end up in one place: one repository to retain and clean up, and one repository a credential has to be able to read for the mounts to work. The cost is that blobs which were mountable for free are uploaded there.<br><br>Left at its sentinel default, this defers to the global `--@rules_img//img/settings:deduplicated_push_blob_repository` flag. Set it to a string to override per target, or to `""` to force "no pinned repository" even when the global flag is set.<br><br>Only consulted when deduplicated push is active (see `deduplicated_push`).   | String | optional |  `"<use global setting>"`  |
| <a id="image_push_spec-deduplicated_push_content"></a>deduplicated_push_content |  What the deduplicated push writes to a shared blob's home repository.<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:deduplicated_push_content` flag. - **`blobs`**: upload the shared blob to its home repository and nothing else. - **`blobs_and_artificial_manifests`**: additionally upload a config blob and create a   manifest referencing the blob and that config in the home repository, before the blob   counts as available to the repositories that share it. For registries that only expose   a blob to other repositories once a manifest references it: with the manifest in place   the blob is served to every repository the caller may read, so the manifest push finds   it there and uploads nothing — with or without a cross-mount.<br><br>The artificial manifest is a real single-layer image manifest (the layer descriptor is the blob's own and the config records the layer's diff id), pushed by digest and untagged. Note that a registry policy which deletes untagged manifests can undo the blob's visibility along with it.<br><br>Only consulted when deduplicated push is active (see `deduplicated_push`).   | String | optional |  `"auto"`  |
| <a id="image_push_spec-destination_file"></a>destination_file |  File containing the push destination as `{registry}/{repository}`.<br><br>The file should contain a single line with the registry and repository separated by the first `/`. For example: `gcr.io/my-project/my-app`.<br><br>The content is read as a literal string without Go template expansion. Trailing newlines and whitespace are stripped.<br><br>Cannot be used together with `registry` or `repository` attributes.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_push_spec-forbid_layer_push"></a>forbid_layer_push |  Whether `img deploy` is forbidden from uploading layer blob bytes.<br><br>When `enabled`, a `bazel run` deploy of this push may only cross-mount layers server-side or skip layers already present; an actual layer upload fails loudly. Use it together with push at build time (which uploads the layer blobs) so a deploy that would re-upload them is caught instead of silently succeeding.<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:forbid_layer_push` flag. - **`enabled`**: forbid layer blob uploads. - **`disabled`**: allow layer blob uploads.   | String | optional |  `"auto"`  |
| <a id="image_push_spec-manifest_tags"></a>manifest_tags |  Per-platform tag templates for multi-platform (`image_index`) pushes.<br><br>Only valid when `image` provides `ImageIndexInfo`. For each entry in this list, the deploy command produces one tag per child manifest in the index by expanding the entry against the platform descriptor of that manifest.<br><br>Available template variables (lowercase):<br><br>- `{{.os}}` — platform OS (e.g. `linux`) - `{{.architecture}}`, `{{.arch}}`, `{{.cpu}}` — architecture (e.g. `amd64`, `arm64`) - `{{.variant}}` — architecture variant (e.g. `v8`), if set<br><br>The tags in `tag` / `tag_list` / `tag_file` continue to point at the index as a whole; `manifest_tags` complement those by publishing additional tags that each resolve to a single child manifest.<br><br>Example:<br><br><pre><code class="language-python">image_push(&#10;    name = "push_multiarch",&#10;    image = ":my_app_index",&#10;    registry = "gcr.io",&#10;    repository = "my-project/my-app",&#10;    tag_list = ["latest", "v1.0.0"],&#10;    manifest_tags = [&#10;        "latest-{{.os}}-{{.architecture}}",&#10;        "v1.0.0-{{.os}}-{{.architecture}}",&#10;    ],&#10;)</code></pre><br><br>Templates are expanded at build time per child manifest, so `build_settings` and stamping variables are available (and override any platform variable of the same name). The expanded tags are emitted as `registry_tag` operations in the deploy manifest, so non-CLI strategies like `bes` can honor them.   | List of strings | optional |  `[]`  |
| <a id="image_push_spec-push_at_build_time"></a>push_at_build_time |  Whether image content is pushed to the registry *during the build*.<br><br>Push at build time wires extra `PushImage` build actions (one per blob, plus a manifest push in `blobs_and_manifests` mode) that upload directly to the registry as a Bazel validation action. See [push at build time](/docs/push-strategies.md#push-at-build-time).<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:push_at_build_time` flag. - **`enabled`**: always push at build time; a push failure fails the build. - **`best_effort`**: push at build time, but a push failure is a warning and does not fail the build. - **`disabled`**: never push at build time.<br><br>When `disabled`, blob cross-mounting via `push_at_build_time_blob_repository` is also not recorded in the deploy manifest, so a later `bazel run` deploy does not try to cross-mount from a staging repository nothing was pushed to.   | String | optional |  `"auto"`  |
| <a id="image_push_spec-push_at_build_time_blob_repository"></a>push_at_build_time_blob_repository |  Staging repository for build-time blob uploads and cross-mounting.<br><br>When non-empty, every image blob (all layers and the config) is pushed to this repository (a "staging" repository within the destination registry) and cross-mounted from there when the manifest is pushed to its real repository.<br><br>Left at its sentinel default, this defers to the global `--@rules_img//img/settings:push_at_build_time_blob_repository` flag. Set it to a string to override per target, or to `""` to force "no staging repository" even when the global flag is set.<br><br>Only takes effect when push at build time is active (see `push_at_build_time`): if push at build time is `disabled` for this target, no cross-mount source is recorded in the deploy manifest even when this is set.   | String | optional |  `"<use global setting>"`  |
| <a id="image_push_spec-push_at_build_time_content"></a>push_at_build_time_content |  What the push-at-build-time actions upload.<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:push_at_build_time_content` flag. - **`blobs`**: push only the layer blobs and the config blob. Manifests/tags are   written afterwards by `image_push` / `multi_deploy`. - **`blobs_and_manifests`**: push the blobs plus the config and manifest(s)/tags,   so the image is fully present in the registry when the build finishes.<br><br>Only consulted when push at build time is active (see `push_at_build_time`).   | String | optional |  `"auto"`  |
| <a id="image_push_spec-push_at_build_time_exec_properties"></a>push_at_build_time_exec_properties |  Execution properties for the `PushImage` build-time push actions.<br><br>Forwarded verbatim as the `execution_requirements` of every `PushImage` action emitted for this target. Defaults to `{"requires-network": "1"}` because the push actions talk to the registry (and are therefore non-hermetic). Override it to add or replace execution properties, for example to route the actions to a specific remote execution pool. Setting it to `{}` removes the `requires-network` marker.<br><br>Only consulted when push at build time is active (see `push_at_build_time`).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{"requires-network": "1"}`  |
| <a id="image_push_spec-push_at_build_time_manifest_repository"></a>push_at_build_time_manifest_repository |  Repository the build-time manifest push writes manifest(s)/config to.<br><br>When non-empty and `push_at_build_time_content` is `blobs_and_manifests`, the build-time manifest push uploads the manifest(s)/index (and, directly, the config) to this repository instead of the image's real repository. This only redirects where manifests are written at build time; it does **not** change where layer blobs are cross-mounted from (that is `push_at_build_time_blob_repository`).<br><br>Left at its sentinel default, this defers to the global `--@rules_img//img/settings:push_at_build_time_manifest_repository` flag. Set it to a string to override per target, or to `""` to force the image's real repository even when the global flag is set.   | String | optional |  `"<use global setting>"`  |
| <a id="image_push_spec-referrers"></a>referrers |  Additional manifests or indexes to push as referrers to the main image.<br><br>Each referrer is pushed to the same registry and repository as the main image, but without tags (referrers are discovered via the OCI referrers API by digest).<br><br>Each target must provide ImageManifestInfo or ImageIndexInfo and must have its `subject` field set to reference the main image being pushed.<br><br>Example: <pre><code class="language-python">image_push(&#10;    name = "push",&#10;    image = ":my_app",&#10;    referrers = [&#10;        ":sbom_manifest",&#10;        ":signature_manifest",&#10;    ],&#10;    registry = "ghcr.io",&#10;    repository = "myorg/myapp",&#10;    tag = "latest",&#10;)</code></pre>   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="image_push_spec-registry"></a>registry |  Registry URL to push the image to.<br><br>Common registries: - Docker Hub: `index.docker.io` - Google Container Registry: `gcr.io` or `us.gcr.io` - GitHub Container Registry: `ghcr.io` - Amazon ECR: `123456789.dkr.ecr.us-east-1.amazonaws.com`<br><br>Subject to [template expansion](/docs/templating.md).   | String | optional |  `""`  |
| <a id="image_push_spec-repository"></a>repository |  Repository path within the registry.<br><br>Subject to [template expansion](/docs/templating.md).   | String | optional |  `""`  |
| <a id="image_push_spec-sign"></a>sign |  Whether `img deploy` signs this image after pushing.<br><br>- **`auto`** (default): defer to the global `--@rules_img//img/settings:sign` flag. - **`enabled`**: always sign; a signing failure (or missing `sign_setting`) fails the deploy. - **`best_effort`**: sign if possible; failures are warnings and do not fail the deploy. - **`disabled`**: never sign.<br><br>The signing plugin is selected by `sign_setting` (or the global `//img/settings:sign_setting`). Signatures are attached as OCI referrers.   | String | optional |  `"auto"`  |
| <a id="image_push_spec-sign_setting"></a>sign_setting |  A `signing_config` target selecting how this image is signed.<br><br>Overrides the global `--@rules_img//img/settings:sign_setting`. Only consulted when signing is enabled (see `sign`).   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_push_spec-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |
| <a id="image_push_spec-strategy"></a>strategy |  Push strategy to use.<br><br>See [push strategies documentation](/docs/push-strategies.md) for detailed information.   | String | optional |  `"auto"`  |
| <a id="image_push_spec-tag"></a>tag |  Tag to apply to the pushed image.<br><br>Optional - if omitted, the image is pushed by digest only.<br><br>Subject to [template expansion](/docs/templating.md).   | String | optional |  `""`  |
| <a id="image_push_spec-tag_file"></a>tag_file |  File containing newline-delimited tags to apply to the pushed image.<br><br>The file should contain one tag per line. Empty lines are ignored. Tags from this file are merged with tags specified via `tag` or `tag_list` attributes.<br><br>Example file content: <pre><code>latest&#10;v1.0.0&#10;stable</code></pre><br><br>Can be combined with `tag` or `tag_list` to merge tags from multiple sources. Each tag is subject to [template expansion](/docs/templating.md).   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_push_spec-tag_list"></a>tag_list |  List of tags to apply to the pushed image.<br><br>Useful for applying multiple tags in a single push:<br><br><pre><code class="language-python">tag_list = ["latest", "v1.0.0", "stable"]</code></pre><br><br>Cannot be used together with `tag`. Can be combined with `tag_file` to merge tags from both sources. Each tag is subject to [template expansion](/docs/templating.md).   | List of strings | optional |  `[]`  |
| <a id="image_push_spec-tracks_content"></a>tracks_content |  When True, the template expansion action depends on the image digest.<br><br>A template string built from a volatile stamp value (e.g. `{{.BUILD_TIMESTAMP}}`) normally freezes on the first build, because Bazel excludes the volatile workspace-status file from the action cache key. With this enabled, the image descriptor becomes an input to the tag-expansion action, so the tag re-stamps whenever the image content (digest) changes, while unchanged content keeps the cached tag.<br><br>The digest is exposed to the `registry`, `repository`, and `tag` templates as `{{.digest}}`. Referencing the digest in the tag is optional: the re-stamp behavior applies whether or not the tag contains it.   | Boolean | optional |  `False`  |


