# rules_img_signer_provenance

A [rules_img](https://github.com/bazel-contrib/rules_img) signer plugin that
attaches **SLSA build provenance** to an image at push time, in the same shape
`docker buildx build --provenance` produces. It implements the
`sign-oci-artifact` subprocess protocol: it reads the subject OCI descriptor on
stdin and writes an OCI image layout tar on stdout. `img deploy` then pushes
that artifact as an OCI 1.1 referrer of the image.

Despite living next to the cosign and notation plugins, it **does not sign
anything** (neither does BuildKit): the attestation is a plain in-toto
statement. To also sign it, put it before a cosign or notation `signing_config`
in a [`signing_chain`](../../docs/image-signing.md#signing-the-image-its-sbom-and-its-provenance)
whose targets include `plugin_artifacts`.

The plugin never talks to a container registry and needs no credentials.

This is an **independent Bazel module**.

## Why at push time?

Facts such as the time of the push, the CI run, or the commit being deployed
change on every build. Recording them in a Bazel action would invalidate the
action cache every time. Because `img deploy` runs the plugin after the build,
the image and all of its layers stay cached and only the small attestation
changes.

## Usage

```python
bazel_dep(name = "rules_img_signer_provenance", version = "<version>")
```

```python
load("@rules_img//img:signing.bzl", "signing_config")

signing_config(
    name = "provenance",
    tool = "@rules_img_signer_provenance",
    args = [
        "--builder-id=https://example.com/my-ci",
        "--record-timestamps",
        "--status-command=tools/workspace_status.sh",
    ],
    # Lets the plugin see where the image is pushed, its platform, and the
    # rules_img version. Without it the plugin only gets a bare descriptor.
    protocol = 2,
    # Attest each platform manifest, like BuildKit does. Do not add
    # "referrers": that would attest the attestations.
    targets = ["roots", "child_manifests"],
)
```

Enable it with `--@rules_img//img/settings:sign_setting=//path:provenance`
(and `--@rules_img//img/settings:sign=enabled`), or per target with the
`sign_setting` attribute of `image_push`.

**Always set `protocol = 2`.** The plugin also works with the original protocol,
where `img deploy` passes only the subject descriptor, but then it cannot know the
image name, platform, or rules_img version and warns about that.

Values that change per run belong in the **deploy-time environment**, not in
`args`: the `signing_config` file is hashed and must stay deterministic. That is
why the commit, timestamps, and the CI context are better taken from a
[status command](#deriving-source-information), `SOURCE_DATE_EPOCH`, `--ci=auto`,
and the `RULES_IMG_PROVENANCE_*` variables than written into a BUILD file.

## What `img deploy` tells the plugin (`protocol = 2`)

With `protocol = 2` the plugin gets a JSON request on stdin instead of the bare
descriptor, and `img deploy` calls it **once per registry, repository, and image
digest** with the tags of every push operation that pushes the image there (so
pushing `:latest` and `:v1` yields one attestation, not two):

| Field | Used for |
| --- | --- |
| `destination.references` | The subject names (`pkg:docker/ghcr.io/example/app@latest`, one per tag). `--subject-name` overrides them. |
| `subject.platform` | `targetPlatform` and the `?platform=` of the subject names, read from the image config. |
| `role`, `parent` | Whether the subject is a root or a manifest of an index (and which). |
| `deploy.rules_img_version` | `runDetails.builder.version.rules_img`. |
| `deploy.invocation_id` | `runDetails.metadata.invocationId` if nothing better is known (see `--invocation-id`): the same for every attestation of one deploy, like the platforms of one BuildKit build. |
| `deploy.started_at` | `startedOn` when timestamps are recorded (honors `SOURCE_DATE_EPOCH`). `--started-on` overrides it. |

Tags attached by separate `registry_tag` operations are not part of the request.

## Deriving source information

Which commit was built is not something Bazel passes to a deploy, and a BUILD file
is the wrong place to write it. Like Bazel's `--workspace_status_command`, the
plugin can run a command at deploy time that prints `KEY VALUE` lines (the first
space separates the key from the value, which may contain spaces):

```sh
#!/bin/sh
echo "BUILD_SCM_REVISION $(git rev-parse HEAD)"
echo "BUILD_SCM_URL $(git config --get remote.origin.url)"
echo "BUILD_SCM_STATUS $(git diff --quiet HEAD && echo clean || echo modified)"
echo "STABLE_TEAM platform"
```

```python
args = ["--status-command=tools/workspace_status.sh", "--status-key=STABLE_TEAM"]
```

| Key | Recorded as |
| --- | --- |
| `BUILD_SCM_REVISION` | `configSource.digest` and `buildkit_metadata.vcs.revision` |
| `BUILD_SCM_URL` | `configSource.uri` and `buildkit_metadata.vcs.source` (credentials redacted) |
| `BUILD_SCM_PATH` | `configSource.path` |
| `BUILD_SCM_REF` | the git ref (`bazel` definitions only: `externalParameters.workspace.ref` and the source dependency's URI) |
| `BUILD_SCM_STATUS` | Anything but `clean` marks `vcs.revision` with `-dirty`, as buildx does. |
| any key named by `--status-key` | `internalParameters.workspace_status` |

* The command is run directly (not through a shell) in `$BUILD_WORKSPACE_DIRECTORY`
  when set (`bazel run` sets it), so it can call `git`. Pass arguments with
  `--status-arg`. A command found on `$PATH` or given as a path both work.
  `$RULES_IMG_PROVENANCE_STATUS_COMMAND` sets it from the environment.
* It runs once per plugin call, so keep it fast. It is stopped after a minute.
* A failing command fails the signing: provenance silently missing its source
  would be misleading.
* Only the keys above and the `--status-key` allowlist are recorded. Nothing else
  the script prints ends up in the attestation.
* Precedence: an explicit `--source-*` flag wins over the command, which wins
  over `--build-info`.

## Choosing the definitions

SLSA leaves what the fields of a provenance mean to its `buildType`. The plugin
can follow one of two sets of definitions, chosen with `--definitions`:

* `buildkit` (the default) mirrors the attestation `docker buildx build
  --provenance` makes, so tools that understand it keep working.
* `bazel` is this plugin's own SLSA provenance, without anything that only makes
  sense for a container build tool: no frontend, no scrubbed build arguments, no
  `buildkit_*` metadata. The requested workspace (repository, ref, path) is an
  external parameter and the resolved commit a `gitCommit` dependency, like in the
  GitHub Actions and Cloud Build build types.

[`slsa-definitions.md`](slsa-definitions.md) defines every field of the `bazel`
definitions, compares them with other build types, and has an example; tests keep
it in sync with the plugin. The sections below describe the `buildkit` layout
unless they say otherwise.

## What is recorded

The attestation manifest matches BuildKit's `oci-artifact=true` form (the
default in current BuildKit):

| Field | Value |
| --- | --- |
| `artifactType` | `application/vnd.docker.attestation.manifest.v1+json` |
| `config` | the OCI empty descriptor (`application/vnd.oci.empty.v1+json`) |
| `subject` | the image manifest it describes (without a platform) |
| layer | one `application/vnd.in-toto+json` blob, annotation `in-toto.io/predicate-type: https://slsa.dev/provenance/v1` |

The layer is an in-toto Statement v1 whose predicate follows BuildKit's SLSA v1
layout (`buildDefinition`, `runDetails`, and the `buildkit_*` metadata keys).
`docker buildx imagetools inspect` finds it through the referrers API.

**Provenance is only as good as its inputs.** BuildKit observes the build; this
plugin cannot. So it records what it is told and does not claim more:

* `buildkit_completeness` is `false/false` unless `--build-info` asserts
  otherwise (and `request` is always `false` in `--mode=min`, which scrubs
  `build-arg:*`/`label:*` parameters).
* `buildkit_hermetic` is never set.
* There is no `buildConfig`, LLB definition, layer map, or resource usage.
* Timestamps are omitted unless requested, so the default output is
  byte-for-byte reproducible.

## Options

| Flag | Meaning |
| --- | --- |
| `--builder-id` | `runDetails.builder.id` (`$RULES_IMG_PROVENANCE_BUILDER_ID`). Empty by default, like BuildKit. |
| `--build-type` | Defaults to BuildKit's SLSA v1 `buildType`. |
| `--frontend` | `request.frontend`. |
| `--artifact-type` | `artifactType` of the attestation manifest. Defaults to `application/vnd.docker.attestation.manifest.v1+json`, which is what BuildKit uses and what `docker buildx imagetools` looks for; with another value that tool will not find the attestation. Must be a media type. |
| `--definitions` | `buildkit` (default) or `bazel`, see [above](#choosing-the-definitions). Some flags exist for one set only: `--frontend`, `--mode` and `--github-event-payload` for `buildkit`, `--target` and `--source-ref` for `bazel`. |
| `--target` | Repeatable Bazel label of what was built and deployed (`bazel` only): `externalParameters.targets`. |
| `--source-ref` | Git ref the source was at, such as `refs/heads/main` (`bazel` only; or `BUILD_SCM_REF` from `--status-command`, or the ref of the GitHub Actions run). |
| `--mode` | `min` (default) or `max`. `min` drops `build-arg:*` and `label:*` parameters, as BuildKit does. |
| `--source-uri`, `--source-commit`, `--source-path` | `configSource` and `buildkit_metadata.vcs`. The commit is 40 (sha1) or 64 (sha256) hex characters. Credentials in URLs are redacted (`xxxxx:xxxxx`). |
| `--status-command`, `--status-arg`, `--status-key` | Derive source information at deploy time, see [above](#deriving-source-information). |
| `--material` | Repeatable `URI` or `URI@algo:hex`, recorded in `resolvedDependencies`. Use `pkg:docker/...` URIs for base images. |
| `--parameter` | Repeatable `key=value` for `request.args`. |
| `--subject-name` | Repeatable image reference, recorded as a `pkg:docker/...?platform=...` purl like BuildKit's subject names. Defaults to the push destination under `protocol = 2`; without a request the name is omitted. |
| `--build-info` | JSON file with facts the build knows (see below). Flags override scalars and append to lists. |
| `--builder-platform` | `internalParameters.builderPlatform`; defaults to the platform of the machine running the plugin. |
| `--invocation-id` | `runDetails.metadata.invocationId` (`$RULES_IMG_PROVENANCE_INVOCATION_ID`). Defaults to `$BUILD_ID`, which `bazel run` sets to the ID of the Bazel invocation; else on GitHub Actions the URL of the run; else the ID `img deploy` generates. |
| `--ci` | `auto` (default) records the CI context, `none` does not. On GitHub Actions this is the context buildx gives BuildKit (`github_event_name`, `github_repository`, `github_run_id`, ...), and the invocation ID defaults to the run URL. |
| `--github-event-payload` | Include the full webhook payload, as buildx does (default true). |
| `--reproducible` | Set `buildkit_reproducible`. |
| `--record-timestamps` | Record `startedOn`/`finishedOn`. `$SOURCE_DATE_EPOCH` also pins and implies them. |
| `--started-on` | RFC 3339 `startedOn` (defaults to `finishedOn`). |

`targetPlatform` comes from the platform in the request (`protocol = 2`).

### `--build-info`

```json
{
  "builder_id": "https://example.com/my-ci",
  "frontend": "bazel",
  "source": {"uri": "https://github.com/example/repo", "commit": "<sha>", "path": "BUILD.bazel"},
  "materials": [{"uri": "pkg:docker/alpine@3.21", "digest": {"sha256": "<hex>"}}],
  "parameters": {"target": "//app:image"},
  "internal_parameters": {"my_key": "my_value"},
  "completeness": {"request": true, "resolvedDependencies": true}
}
```

Custom `internal_parameters` (and CI context) keys must not collide with
BuildKit's own (`builderPlatform`, `targetPlatform`, `dockerfileVersion`,
`buildConfig`).

## Building from source (git_override / local path)

Without a populated `prebuilt_lockfile.json` the module builds the plugin from
source. That needs `rules_go` and `gazelle`, which the release archive does not
depend on. The only Go dependency is `go-containerregistry`.
