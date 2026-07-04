"""Common attributes shared by push/load rules and their library counterparts."""

load("@bazel_skylib//rules:common_settings.bzl", "BuildSettingInfo")
load("//img/private/providers:deploy_info.bzl", "DeployInfo")
load("//img/private/providers:index_info.bzl", "ImageIndexInfo")
load("//img/private/providers:load_settings_info.bzl", "LoadSettingsInfo")
load("//img/private/providers:manifest_info.bzl", "ImageManifestInfo")
load("//img/private/providers:push_settings_info.bzl", "PushSettingsInfo")
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
    tag = attr.string(
        doc = """Tag to apply when loading the image.

Optional - if omitted, the image is loaded without a tag.

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
)
