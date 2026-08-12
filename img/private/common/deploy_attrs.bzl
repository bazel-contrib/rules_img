"""Common attributes shared by push/load rules and their library counterparts."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/common:deploy_helpers.bzl", "USE_GLOBAL_SETTING")
load("//img/private/providers:deploy_info.bzl", "DeployInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:load_settings_info.bzl", "LoadSettingsInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:push_at_build_time_settings_info.bzl", "PushAtBuildTimeSettingsInfo")
load("//img/private/providers:push_settings_info.bzl", "PushSettingsInfo")
load("//img/private/providers:signing_config_info.bzl", "SigningConfigInfo")
load("//img/private/providers:stamp_setting_info.bzl", "StampSettingInfo")

COMMON_PUSH_ATTRS = dict(
    registry = attr.string(
        doc = """Registry URL to push the image to.

Common registries:
- Docker Hub: `index.docker.io`
- Google Container Registry: `gcr.io` or `us.gcr.io`
- GitHub Container Registry: `ghcr.io`
- Amazon ECR: `123456789.dkr.ecr.us-east-1.amazonaws.com`

Subject to [template expansion](/docs/templating.md).
""",
    ),
    repository = attr.string(
        doc = """Repository path within the registry.

Subject to [template expansion](/docs/templating.md).
""",
    ),
    tag = attr.string(
        doc = """Tag to apply to the pushed image.

Optional - if omitted, the image is pushed by digest only.

Subject to [template expansion](/docs/templating.md).
""",
    ),
    tag_list = attr.string_list(
        doc = """List of tags to apply to the pushed image.

Useful for applying multiple tags in a single push:

```python
tag_list = ["latest", "v1.0.0", "stable"]
```

Cannot be used together with `tag`. Can be combined with `tag_file` to merge tags from both sources.
Each tag is subject to [template expansion](/docs/templating.md).
""",
    ),
    manifest_tags = attr.string_list(
        doc = """Per-platform tag templates for multi-platform (`image_index`) pushes.

Only valid when `image` provides `ImageIndexInfo`. For each entry in this list, the
deploy command produces one tag per child manifest in the index by expanding the
entry against the platform descriptor of that manifest.

Available template variables (lowercase):

- `{{.os}}` — platform OS (e.g. `linux`)
- `{{.architecture}}`, `{{.arch}}`, `{{.cpu}}` — architecture (e.g. `amd64`, `arm64`)
- `{{.variant}}` — architecture variant (e.g. `v8`), if set

The tags in `tag` / `tag_list` / `tag_file` continue to point at the index as a
whole; `manifest_tags` complement those by publishing additional tags that each
resolve to a single child manifest.

Example:

```python
image_push(
    name = "push_multiarch",
    image = ":my_app_index",
    registry = "gcr.io",
    repository = "my-project/my-app",
    tag_list = ["latest", "v1.0.0"],
    manifest_tags = [
        "latest-{{.os}}-{{.architecture}}",
        "v1.0.0-{{.os}}-{{.architecture}}",
    ],
)
```

Templates are expanded at build time per child manifest, so `build_settings`
and stamping variables are available (and override any platform variable of
the same name). The expanded tags are emitted as `registry_tag` operations
in the deploy manifest, so non-CLI strategies like `bes` can honor them.
""",
    ),
    tag_file = attr.label(
        doc = """File containing newline-delimited tags to apply to the pushed image.

The file should contain one tag per line. Empty lines are ignored. Tags from this file
are merged with tags specified via `tag` or `tag_list` attributes.

Example file content:
```
latest
v1.0.0
stable
```

Can be combined with `tag` or `tag_list` to merge tags from multiple sources.
Each tag is subject to [template expansion](/docs/templating.md).
""",
        allow_single_file = True,
    ),
    destination_file = attr.label(
        doc = """File containing the push destination as `{registry}/{repository}`.

The file should contain a single line with the registry and repository separated by
the first `/`. For example: `gcr.io/my-project/my-app`.

The content is read as a literal string without Go template expansion. Trailing
newlines and whitespace are stripped.

Cannot be used together with `registry` or `repository` attributes.
""",
        allow_single_file = True,
    ),
    referrers = attr.label_list(
        doc = """Additional manifests or indexes to push as referrers to the main image.

Each referrer is pushed to the same registry and repository as the main image,
but without tags (referrers are discovered via the OCI referrers API by digest).

Each target must provide ImageManifestInfo or ImageIndexInfo and must have its
`subject` field set to reference the main image being pushed.

Example:
```python
image_push(
    name = "push",
    image = ":my_app",
    referrers = [
        ":sbom_manifest",
        ":signature_manifest",
    ],
    registry = "ghcr.io",
    repository = "myorg/myapp",
    tag = "latest",
)
```
""",
        providers = [[ImageManifestInfo], [ImageIndexInfo]],
    ),
    cross_mount_from = attr.label(
        doc = "An image_push target whose layers may be cross-mounted during push.",
        providers = [DeployInfo],
    ),
    strategy = attr.string(
        doc = """Push strategy to use.

See [push strategies documentation](/docs/push-strategies.md) for detailed information.
""",
        default = "auto",
        values = ["auto", "eager", "lazy", "cas_registry", "bes"],
    ),
    build_settings = attr.string_keyed_label_dict(
        doc = """Build settings for template expansion.

Maps template variable names to string_flag targets. These values can be used in
registry, repository, and tag attributes using `{{.VARIABLE_NAME}}` syntax (Go template).

Example:
```python
build_settings = {
    "REGISTRY": "//settings:docker_registry",
    "VERSION": "//settings:app_version",
}
```

See [template expansion](/docs/templating.md) for more details.
""",
        providers = [BuildSettingInfo],
    ),
    stamp = attr.string(
        doc = """Controls build stamping for template expansion.

- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting.
- **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag.
- **`disabled`**: Never include stamp information.

See [template expansion](/docs/templating.md) for available stamp variables.
""",
        default = "auto",
        values = ["auto", "force", "disabled"],
    ),
    tracks_content = attr.bool(
        doc = """When True, the template expansion action depends on the image digest.

A template string built from a volatile stamp value (e.g. `{{.BUILD_TIMESTAMP}}`) normally
freezes on the first build, because Bazel excludes the volatile workspace-status
file from the action cache key. With this enabled, the image descriptor becomes
an input to the tag-expansion action, so the tag re-stamps whenever the image
content (digest) changes, while unchanged content keeps the cached tag.

The digest is exposed to the `registry`, `repository`, and `tag` templates
as `{{.digest}}`. Referencing the digest in the tag is optional: the re-stamp
behavior applies whether or not the tag contains it.
""",
        default = False,
    ),
    sign = attr.string(
        doc = """Whether `img deploy` signs this image after pushing.

- **`auto`** (default): defer to the global `--@rules_img//img/settings:sign` flag.
- **`enabled`**: always sign; a signing failure (or missing `sign_setting`) fails the deploy.
- **`best_effort`**: sign if possible; failures are warnings and do not fail the deploy.
- **`disabled`**: never sign.

The signing plugin is selected by `sign_setting` (or the global
`//img/settings:sign_setting`). Signatures are attached as OCI referrers.
""",
        default = "auto",
        values = ["auto", "enabled", "disabled", "best_effort"],
    ),
    sign_setting = attr.label(
        doc = """A `signing_config` target selecting how this image is signed.

Overrides the global `--@rules_img//img/settings:sign_setting`. Only consulted
when signing is enabled (see `sign`).
""",
        providers = [SigningConfigInfo],
    ),
    push_at_build_time = attr.string(
        doc = """Whether image content is pushed to the registry *during the build*.

Push at build time wires extra `PushImage` build actions (one per blob, plus a
manifest push in `blobs_and_manifests` mode) that upload directly to the registry
as a Bazel validation action. See
[push at build time](/docs/push-strategies.md#push-at-build-time).

- **`auto`** (default): defer to the global `--@rules_img//img/settings:push_at_build_time` flag.
- **`enabled`**: always push at build time; a push failure fails the build.
- **`best_effort`**: push at build time, but a push failure is a warning and does not fail the build.
- **`disabled`**: never push at build time.

When `disabled`, blob cross-mounting via `push_at_build_time_blob_repository` is
also not recorded in the deploy manifest, so a later `bazel run` deploy does not
try to cross-mount from a staging repository nothing was pushed to.
""",
        default = "auto",
        values = ["auto", "disabled", "best_effort", "enabled"],
    ),
    push_at_build_time_content = attr.string(
        doc = """What the push-at-build-time actions upload.

- **`auto`** (default): defer to the global `--@rules_img//img/settings:push_at_build_time_content` flag.
- **`blobs`**: push only the layer blobs and the config blob. Manifests/tags are
  written afterwards by `image_push` / `multi_deploy`.
- **`blobs_and_manifests`**: push the blobs plus the config and manifest(s)/tags,
  so the image is fully present in the registry when the build finishes.

Only consulted when push at build time is active (see `push_at_build_time`).
""",
        default = "auto",
        values = ["auto", "blobs", "blobs_and_manifests"],
    ),
    push_at_build_time_blob_repository = attr.string(
        doc = """Staging repository for build-time blob uploads and cross-mounting.

When non-empty, every image blob (all layers and the config) is pushed to this
repository (a "staging" repository within the destination registry) and
cross-mounted from there when the manifest is pushed to its real repository.

Left at its sentinel default, this defers to the global
`--@rules_img//img/settings:push_at_build_time_blob_repository` flag. Set it to a
string to override per target, or to `""` to force "no staging repository" even
when the global flag is set.

Only takes effect when push at build time is active (see `push_at_build_time`):
if push at build time is `disabled` for this target, no cross-mount source is
recorded in the deploy manifest even when this is set.
""",
        default = USE_GLOBAL_SETTING,
    ),
    push_at_build_time_manifest_repository = attr.string(
        doc = """Repository the build-time manifest push writes manifest(s)/config to.

When non-empty and `push_at_build_time_content` is `blobs_and_manifests`, the
build-time manifest push uploads the manifest(s)/index (and, directly, the
config) to this repository instead of the image's real repository. This only
redirects where manifests are written at build time; it does **not** change where
layer blobs are cross-mounted from (that is `push_at_build_time_blob_repository`).

Left at its sentinel default, this defers to the global
`--@rules_img//img/settings:push_at_build_time_manifest_repository` flag. Set it
to a string to override per target, or to `""` to force the image's real
repository even when the global flag is set.
""",
        default = USE_GLOBAL_SETTING,
    ),
    forbid_layer_push = attr.string(
        doc = """Whether `img deploy` is forbidden from uploading layer blob bytes.

When `enabled`, a `bazel run` deploy of this push may only cross-mount layers
server-side or skip layers already present; an actual layer upload fails loudly.
Use it together with push at build time (which uploads the layer blobs) so a
deploy that would re-upload them is caught instead of silently succeeding.

- **`auto`** (default): defer to the global `--@rules_img//img/settings:forbid_layer_push` flag.
- **`enabled`**: forbid layer blob uploads.
- **`disabled`**: allow layer blob uploads.
""",
        default = "auto",
        values = ["auto", "disabled", "enabled"],
    ),
    deduplicated_push = attr.string(
        doc = """Whether `img deploy` uploads a blob several repositories need only once.

When `enabled`, the deploy first asks the registry which manifests it already
holds, then uploads each remaining layer that more than one destination repository
needs to just one of them — the first alphabetically — and cross-mounts it into the
others. Meant for registries that keep a separate blob store per repository name,
where pushing several images that share their layers otherwise uploads every shared
layer once per repository. See
[deduplicated push](/docs/push-strategies.md#deduplicated-push).

**`enabled` only works on a registry that supports cross-repository blob mounting.**
A push that opted in fails loudly where mounting is refused, rather than silently
uploading the blob into every repository. See the
[registry support matrix](/docs/registry-support.md).

- **`auto`** (default): defer to the global `--@rules_img//img/settings:deduplicated_push` flag.
- **`enabled`**: deduplicate blob uploads, and fail the push if a layer this deploy
  uploaded to a home repository cannot be cross-mounted from there.
- **`best_effort`**: deduplicate blob uploads, but upload a layer's bytes the ordinary
  way if the registry refuses to mount it (and treat a failed upload to a home
  repository as a warning). The deduplication is attempted without the deploy
  depending on it, which is what makes it usable on a registry you have not probed.
- **`disabled`**: push each manifest independently.

The setting travels with this push operation, so a deploy that merges several of
them (`multi_deploy`, or an image with several `push_specs`) can mix the two: the
operations that opted in are planned together and cross-mount between each other,
while the rest are pushed exactly as they would be with the setting off.
""",
        default = "auto",
        values = ["auto", "disabled", "best_effort", "enabled"],
    ),
    deduplicated_push_blob_repository = attr.string(
        doc = """Repository the deduplicated push uploads this target's shared blobs to.

When non-empty, every blob the deduplicated push shares between repositories is
uploaded to this repository (within the destination registry) and cross-mounted from
there. It takes precedence over every home repository the deploy would have picked
itself — including a repository the registry already serves the blob from — so that
shared blobs all end up in one place: one repository to retain and clean up, and one
repository a credential has to be able to read for the mounts to work. The cost is
that blobs which were mountable for free are uploaded there.

Left at its sentinel default, this defers to the global
`--@rules_img//img/settings:deduplicated_push_blob_repository` flag. Set it to a
string to override per target, or to `""` to force "no pinned repository" even when
the global flag is set.

Only consulted when deduplicated push is active (see `deduplicated_push`).
""",
        default = USE_GLOBAL_SETTING,
    ),
    deduplicated_push_content = attr.string(
        doc = """What the deduplicated push writes to a shared blob's home repository.

- **`auto`** (default): defer to the global `--@rules_img//img/settings:deduplicated_push_content` flag.
- **`blobs`**: upload the shared blob to its home repository and nothing else.
- **`blobs_and_artificial_manifests`**: additionally upload a config blob and create a
  manifest referencing the blob and that config in the home repository, before the blob
  counts as available to the repositories that share it. For registries that only expose
  a blob to other repositories once a manifest references it: with the manifest in place
  the blob is served to every repository the caller may read, so the manifest push finds
  it there and uploads nothing — with or without a cross-mount.

The artificial manifest is a real single-layer image manifest (the layer descriptor is
the blob's own and the config records the layer's diff id), pushed by digest and
untagged. Note that a registry policy which deletes untagged manifests can undo the
blob's visibility along with it.

Only consulted when deduplicated push is active (see `deduplicated_push`).
""",
        default = "auto",
        values = ["auto", "blobs", "blobs_and_artificial_manifests"],
    ),
    push_at_build_time_exec_properties = attr.string_dict(
        doc = """Execution properties for the `PushImage` build-time push actions.

Forwarded verbatim as the `execution_requirements` of every `PushImage` action
emitted for this target. Defaults to `{"requires-network": "1"}` because the push
actions talk to the registry (and are therefore non-hermetic). Override it to add
or replace execution properties, for example to route the actions to a specific
remote execution pool. Setting it to `{}` removes the `requires-network` marker.

Only consulted when push at build time is active (see `push_at_build_time`).
""",
        default = {"requires-network": "1"},
    ),
    _push_at_build_time_settings = attr.label(
        default = Label("//img/private/settings:push_at_build_time"),
        providers = [PushAtBuildTimeSettingsInfo],
    ),
    _sign = attr.label(
        default = Label("//img/settings:sign"),
        providers = [BuildSettingInfo],
    ),
    _sign_setting = attr.label(
        default = Label("//img/settings:sign_setting"),
        providers = [SigningConfigInfo],
    ),
    _push_settings = attr.label(
        default = Label("//img/private/settings:push"),
        providers = [PushSettingsInfo],
    ),
    _stamp_settings = attr.label(
        default = Label("//img/private/settings:stamp"),
        providers = [StampSettingInfo],
    ),
    _destination_registry = attr.label(
        default = Label("//img/settings:destination_registry"),
        providers = [BuildSettingInfo],
    ),
)

COMMON_LOAD_ATTRS = dict(
    daemon = attr.string(
        doc = """Container daemon to use for loading the image.

Available options:
- **`auto`** (default): Uses the global default setting (usually `docker`)
- **`containerd`**: Loads directly into containerd namespace. Supports multi-platform images
  and incremental loading.
- **`docker`**: Loads via Docker daemon. When Docker uses containerd storage (23.0+),
  loads directly into containerd. Otherwise falls back to `docker image load` command which
  is slower and limited to single-platform images.
- **`podman`**: Loads via Podman daemon using `podman image load` command. Similar to Docker
  fallback mode, this is slower than containerd and limited to single-platform images.
- **`containerization`**: Loads via Apple's Containerization framework using `container image load`.
  Reads a unified OCI+Docker tar from stdin.
- **`tar`**: Does not load into any daemon. Instead, streams the unified OCI+Docker tar to stdout.
  Useful for piping to other tools or saving to a file.
- **`generic`**: Loads via a custom container runtime. The loader will invoke the command
  specified in the `LOADER_BINARY` environment variable with `image load` subcommands. For example,
  if `LOADER_BINARY=nerdctl`, it will run `nerdctl image load`.
  Requires `LOADER_BINARY` to be set at runtime.

The best performance is achieved with:
- Direct containerd access (daemon = "containerd")
- Docker 23.0+ with containerd storage enabled and accessible containerd socket
""",
        default = "auto",
        values = ["auto", "docker", "containerd", "podman", "containerization", "tar", "generic"],
    ),
    registry = attr.string(
        doc = """Registry component of the image name to load.

Optional. When set, `repository` must also be set, and each entry in
`tag` / `tag_list` / `tag_file` is treated as a bare tag: the loaded image name
is reconstructed as `{registry}/{repository}:{tag}` (mirroring `image_push`).
May include a port (e.g. `docker.mycompany.tld:1234`).

When omitted but `repository` is set, the global
`--@rules_img//img/settings:destination_registry` flag is used as a fallback
(again mirroring `image_push`).

When omitted together with `repository`, the tags are used verbatim as full
image references, preserving the `rules_oci`-compatible behavior. In this mode
the `destination_registry` fallback does not apply.

Whichever way the name is put together, it is then used as written: it never
goes through Docker's reference normalization (which would add `index.docker.io`
and the `library/` namespace), and a name that is not a valid image reference
fails the build.

Subject to [template expansion](/docs/templating.md).
""",
    ),
    repository = attr.string(
        doc = """Repository component of the image name to load.

Optional. Must be set together with `registry` (see `registry` for details).

Subject to [template expansion](/docs/templating.md).
""",
    ),
    tag = attr.string(
        doc = """Tag to apply when loading the image.

Optional - if omitted, the image is loaded without a name.

When `registry`/`repository` are set, this is a bare tag (e.g. `latest`);
otherwise it is a full image reference (e.g. `my-app:latest`) - a reference
written without a tag (e.g. `my-app`) is loaded as `my-app:latest`.

Subject to [template expansion](/docs/templating.md).
""",
    ),
    tag_list = attr.string_list(
        doc = """List of tags to apply when loading the image.

Useful for applying multiple tags in a single load:

```python
tag_list = ["latest", "v1.0.0", "stable"]
```

Cannot be used together with `tag`. Can be combined with `tag_file` to merge tags from both sources.
Each tag is subject to [template expansion](/docs/templating.md).
""",
    ),
    tag_file = attr.label(
        doc = """File containing newline-delimited tags to apply when loading the image.

The file should contain one tag per line. Empty lines are ignored. Tags from this file
are merged with tags specified via `tag` or `tag_list` attributes.

Example file content:
```
latest
v1.0.0
stable
```

Can be combined with `tag` or `tag_list` to merge tags from multiple sources.
Each tag is subject to [template expansion](/docs/templating.md).
""",
        allow_single_file = True,
    ),
    strategy = attr.string(
        doc = """Strategy for handling image layers during load.

Available strategies:
- **`auto`** (default): Uses the global default load strategy
- **`eager`**: Downloads all layers during the build phase. Ensures all layers are
  available locally before running the load command.
- **`lazy`**: Downloads layers only when needed during the load operation. More
  efficient for large images where some layers might already exist in the daemon.
""",
        default = "auto",
        values = ["auto", "eager", "lazy"],
    ),
    build_settings = attr.string_keyed_label_dict(
        doc = """Build settings for template expansion.

Maps template variable names to string_flag targets. These values can be used in
tag attributes using `{{.VARIABLE_NAME}}` syntax (Go template).

See [template expansion](/docs/templating.md) for more details.
""",
        providers = [BuildSettingInfo],
    ),
    stamp = attr.string(
        doc = """Controls build stamping for template expansion.

- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting.
- **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag.
- **`disabled`**: Never include stamp information.

See [template expansion](/docs/templating.md) for available stamp variables.
""",
        default = "auto",
        values = ["auto", "force", "disabled"],
    ),
    tracks_content = attr.bool(
        doc = """When True, the template expansion action depends on the image digest.

A template string built from a volatile stamp value (e.g. `{{.BUILD_TIMESTAMP}}`) normally
freezes on the first build, because Bazel excludes the volatile workspace-status
file from the action cache key. With this enabled, the image descriptor becomes
an input to the tag-expansion action, so the tag re-stamps whenever the image
content (digest) changes, while unchanged content keeps the cached tag.

The digest is exposed to the `tag` templates as `{{.digest}}`. Referencing the
digest in the tag is optional: the re-stamp behavior applies whether or not the
tag contains it.
""",
        default = False,
    ),
    _load_settings = attr.label(
        default = Label("//img/private/settings:load"),
        providers = [LoadSettingsInfo],
    ),
    _stamp_settings = attr.label(
        default = Label("//img/private/settings:stamp"),
        providers = [StampSettingInfo],
    ),
    _destination_registry = attr.label(
        default = Label("//img/settings:destination_registry"),
        providers = [BuildSettingInfo],
    ),
)
