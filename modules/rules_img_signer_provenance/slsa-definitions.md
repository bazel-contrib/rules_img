# SLSA definitions

The provenance plugin writes [SLSA provenance v1](https://slsa.dev/spec/v1.1/provenance)
for images that `img deploy` publishes. SLSA leaves the meaning of a provenance's
`externalParameters`, `internalParameters` and `resolvedDependencies` to its
[`buildType`](https://slsa.dev/spec/v1.1/provenance#buildType), a URI that points
at a document like this one. The plugin can follow one of two sets of definitions,
selected with `--definitions`:

| `--definitions` | `buildType` | Defined by |
| --- | --- | --- |
| `buildkit` (default) | `https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-definitions.md` | [BuildKit](https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-definitions.md). The attestation reads like the one `docker buildx build --provenance` makes, so tools that understand it keep working. |
| `bazel` | `https://github.com/bazel-contrib/rules_img/blob/main/modules/rules_img_signer_provenance/slsa-definitions.md#bazel-v1` | This document, [below](#bazel-v1). |

The `bazel` definitions describe the publication of an image built by Bazel and
drop everything that only makes sense for a container build tool: there is no
frontend, no build arguments to scrub, no LLB, and no `buildkit_*` metadata.

The keywords MUST, SHOULD and MAY are used as in
[RFC 2119](https://www.rfc-editor.org/rfc/rfc2119).

## Bazel v1

```jsonc
"buildType": "https://github.com/bazel-contrib/rules_img/blob/main/modules/rules_img_signer_provenance/slsa-definitions.md#bazel-v1"
```

A provenance has two parts. `buildDefinition` is what was asked for and what it was
built from: the `buildType`, the external and internal parameters, and the
resolved dependencies. `runDetails` is about the run itself: the builder and the
metadata. Each field below is written as its path from the predicate.

### What this provenance attests

This `buildType` describes **the publication of an image by `img deploy`**, run
from a Bazel workspace at a given source revision: that image digest was pushed to
that repository by that builder, from that source, with those parameters.

It does **not** describe the Bazel build that produced the image. The plugin runs
while the image is pushed, after Bazel has finished, and cannot observe the
build's actions or inputs; it only knows what `img deploy` tells it and what its
caller passes in. What ties the published digest to the source is Bazel's
hermeticity and the image's reproducibility, which the producer can assert with
`internalParameters.reproducible` but which this provenance does not prove.

What the provenance buys is a signed (once a signer such as cosign signs it, see
`signing_chain`) and queryable record of *who published what, from which revision,
when*, in a format policy engines already understand.

A verifier MUST NOT read more into the provenance than that, and SHOULD reject
external parameters it does not recognize. New external parameters are added
without a new version only if they leave the meaning of the existing ones unchanged.

### Versioning

The fragment of the `buildType` (`bazel-v1`) names the section of this document
that defines it. A change that alters the meaning of an existing field gets a new
section and a new `buildType`; adding an optional field does not.

### `buildDefinition.buildType`

* Ref: https://slsa.dev/spec/v1.1/provenance#buildType
* Always included.

The `buildType` above. `--build-type` replaces it, for provenance that follows
these definitions under a different identity.

### `buildDefinition.externalParameters`

* Ref: https://slsa.dev/spec/v1.1/provenance#externalParameters
* Always included, and an object. Every field is omitted when it is empty.

What the caller of the deploy controls. A verifier SHOULD check the ones it cares
about against its expectations, and MUST treat them as untrusted input.

#### `buildDefinition.externalParameters.workspace`

The Bazel workspace the image was built from, as requested. The resolved commit
is in [`resolvedDependencies`](#builddefinitionresolveddependencies).

* `buildDefinition.externalParameters.workspace.repository`: the URI of the git
  repository, with credentials replaced by `xxxxx:xxxxx`. Set by
  `--source-uri`, `BUILD_SCM_URL` of `--status-command`, or `source.uri` of
  `--build-info`.
* `buildDefinition.externalParameters.workspace.ref`: the git ref that was built,
  such as `refs/heads/main`. Set by `--source-ref`, `BUILD_SCM_REF`, or `source.ref`;
  on GitHub Actions it defaults to the ref of the run.
* `buildDefinition.externalParameters.workspace.path`: the path of the Bazel
  workspace (the directory with the `MODULE.bazel`) inside the repository, without
  a leading or trailing slash. Empty for the root. Set by `--source-path`,
  `BUILD_SCM_PATH`, or `source.path`.

```json
"workspace": {
  "repository": "https://github.com/bazel-contrib/rules_img",
  "ref": "refs/heads/main",
  "path": "modules/rules_img_services"
}
```

#### `buildDefinition.externalParameters.targets`

The Bazel labels of what was built and deployed, in the order given and without
duplicates. Set by `--target` and `targets` of `--build-info`, which are what the
caller says: the plugin cannot see the Bazel invocation. When the
[build info](#bazel-target-info) of the deploy is available, the label of the
deploy rule (the `image_push` or `multi_deploy` that was run) is added last. That
one is recorded by the rule itself, and a verifier can trust it as far as it
trusts the rules.

#### `buildDefinition.externalParameters.parameters`

Other parameters of the invocation as `key`/`value` strings, such as the Bazel
configs or flags that matter to the result. Set by `--parameter` and `parameters`
of `--build-info`; the keys have no meaning of their own. The plugin does not
inspect them and a producer MUST NOT put secrets in them.

#### `buildDefinition.externalParameters.workspaceStatus`

The values of the keys named with `--status-key` in the output of
`--status-command`. Nothing else of the command's output is recorded. They are
external parameters because the script that prints them belongs to the workspace.

### `buildDefinition.internalParameters`

* Ref: https://slsa.dev/spec/v1.1/provenance#internalParameters
* Always included.

What the builder controls and a consumer can trust as far as it trusts the
builder.

#### `buildDefinition.internalParameters.builderPlatform`

The platform (`os/arch`) of the machine that ran `img deploy`. Always included.
This is not the platform of the image, which is recorded in `targetPlatform`.

#### `buildDefinition.internalParameters.targetPlatform`

The platform of the image the provenance is about, such as `linux/arm64`, taken
from the image's config. Included for a single-platform image and omitted for an
image index and for artifacts that have no platform. The platform is normalized
like containerd and BuildKit do: the default variant is dropped, so `arm64/v8` is
`arm64` and `amd64/v1` is `amd64`.

#### `buildDefinition.internalParameters.pushStrategy`

The push strategy of the deploy, such as `eager`. Included when `img deploy`
supplies it.

#### `buildDefinition.internalParameters.reproducible`

`true` if the producer asserts that the same inputs produce the same image
digest. Omitted otherwise; it is never `false`. Set by `--reproducible` or
`reproducible` of `--build-info`. A claim of the producer, not a result.

#### `buildDefinition.internalParameters.ci`

The CI environment, when there is one and `--ci=auto`. Like the GitHub Actions
build type, it only holds what describes the environment and is not already
recorded elsewhere (the repository and ref are in the workspace, the workflow in
`builder.id`, the run in `invocationId`), and it prefers numeric IDs, which
survive a rename, to names.

* `buildDefinition.internalParameters.ci.system`: `github-actions`, the only
  system supported so far.
* `buildDefinition.internalParameters.ci.eventName`: the event that triggered the
  workflow, such as `push`.
* `buildDefinition.internalParameters.ci.actorId`: the numeric ID of the user that
  triggered the run.
* `buildDefinition.internalParameters.ci.repositoryId`: the numeric ID of the
  repository.
* `buildDefinition.internalParameters.ci.repositoryOwnerId`: the numeric ID of the
  user or organization that owns the repository.
* `buildDefinition.internalParameters.ci.refType`: `branch` or `tag`.
* `buildDefinition.internalParameters.ci.refProtected`: `true` if the ref is
  protected, as a string.
* `buildDefinition.internalParameters.ci.runner`: the runner that ran the
  deploy.
* `buildDefinition.internalParameters.ci.runner.environment`: `github-hosted` or
  `self-hosted`.
* `buildDefinition.internalParameters.ci.runner.os`,
  `buildDefinition.internalParameters.ci.runner.arch`,
  `buildDefinition.internalParameters.ci.runner.imageOs` and
  `buildDefinition.internalParameters.ci.runner.imageVersion`: the operating
  system and architecture of the runner and the version of its image.

Each group is included only if all of its values are available. `--build-info`
can add internal parameters of its own under other names.

### `buildDefinition.resolvedDependencies`

* Ref: https://slsa.dev/spec/v1.1/provenance#resolvedDependencies
* Included when there is something to list.

The inputs the producer resolved. In this order:

1. The source revision, when a commit is known (`--source-commit`,
   `BUILD_SCM_REVISION`, or `source.commit`):
   * `buildDefinition.resolvedDependencies[].name` is `source`.
   * `buildDefinition.resolvedDependencies[].uri` is the repository as a
     [SPDX download location](https://spdx.github.io/spdx-spec/v2.3/package-information/#77-package-download-location-field),
     `git+<repository>@<ref>`, without the `@<ref>` if no ref is known.
   * `buildDefinition.resolvedDependencies[].digest.gitCommit` is the commit.
   * `buildDefinition.resolvedDependencies[].annotations` holds facts about the
     dependency. It is absent unless one applies.
   * `buildDefinition.resolvedDependencies[].annotations.modified` is `true` if
     the working tree had uncommitted changes (`BUILD_SCM_STATUS` other than
     `clean`), and absent otherwise. A revision with local changes is not the
     revision the commit names.
2. Anything else given with `--material` or `materials` of `--build-info`, with
   the `uri` and `digest` as given (the `uri` without credentials) and the
   `name` that `--build-info` assigns, if any. The `digest` maps the name of a hash
   algorithm to a hex value (`buildDefinition.resolvedDependencies[].digest`, for
   example `buildDefinition.resolvedDependencies[].digest.sha256`). Use a
   [package URL](https://github.com/package-url/purl-spec) such as
   `pkg:docker/alpine@3.21` for a container image.

```json
"resolvedDependencies": [
  {
    "name": "source",
    "uri": "git+https://github.com/bazel-contrib/rules_img@refs/heads/main",
    "digest": { "gitCommit": "55f1937b77c46a5dfb8cc4f8c4938b8e729c05a1" }
  }
]
```

#### Bazel target info

When the `signing_config` of the plugin sets `build_info = True` (which needs
`protocol = 2`), `img deploy` hands the plugin a file naming the Bazel targets the
deploy was built from. An aspect collects them below the `image_push` or
`multi_deploy` rule, so the cost is limited to deploys that ask for it, and
the file holds labels and digests only (no file contents). The plugin records, for
the subject it signs:

3. The pulled images the subject is based on, followed down base images of base
   images:
   * `buildDefinition.resolvedDependencies[].name` is the label of the target that
     pulled the image.
   * `buildDefinition.resolvedDependencies[].uri` is the
     [package URL](https://github.com/package-url/purl-spec) of the image, such as
     `pkg:docker/gcr.io/distroless/cc@latest`, and
     `buildDefinition.resolvedDependencies[].digest.sha256` is its digest.
   * `buildDefinition.resolvedDependencies[].annotations.kind` is `base-image`.
   * `buildDefinition.resolvedDependencies[].annotations.registry`,
     `buildDefinition.resolvedDependencies[].annotations.repository` and
     `buildDefinition.resolvedDependencies[].annotations.tag` are where it was pulled
     from (the first of its registries) and the tag, if it has one.
4. Unless `--bazel-info=bases`, the `image_manifest` and `image_index` targets the
   subject consists of (for an index, its images) and their layer targets, the layers in the order the image stacks them (bottom first). Their
   `name` is the label, their `digest.sha256` the digest of the manifest, index, or
   layer blob, and `buildDefinition.resolvedDependencies[].annotations.kind` is
   `image`, `index`, or `layer`.
   `buildDefinition.resolvedDependencies[].annotations.platform` is the platform
   of an `image` (normalized like `targetPlatform`); an `index` and a `layer` have none.
   `buildDefinition.resolvedDependencies[].annotations.configuration` is the
   configuration the target was built in: the name of its output directory, such as
   `k8-fastbuild`. The same target built in two configurations is listed twice.

`--bazel-info=none` records none of these, but still adds the deploy rule to
`externalParameters.targets`. An image index and each of its
children list their own closure.

The Bazel command line is not recorded: `bazel run` replaces the Bazel client with
the deploy binary, which therefore never sees the arguments. What identifies the
invocation is its ID (see `runDetails.metadata.invocationId`), and the flags that
matter can be named with `--parameter`.

### `runDetails.builder`

* Ref: https://slsa.dev/spec/v1.1/provenance#builder
* Always included.

`runDetails.builder.id` identifies the entity that generated the provenance and
whose behavior a verifier trusts. It is set by `--builder-id` or
`$RULES_IMG_PROVENANCE_BUILDER_ID` and is empty if neither is. A producer SHOULD set
it, and on GitHub Actions to `github.server_url + "/" + github.workflow_ref` (the
workflow that runs the deploy), like the GitHub Actions build type.

`runDetails.builder.version` names the versions of the builder's components, and
`runDetails.builder.version.rules_img` is the version of `rules_img` (`img`) that
ran the deploy. Included when `img deploy` supplies it (protocol 2).

### `runDetails.metadata`

* Ref: https://slsa.dev/spec/v1.1/provenance#metadata

`runDetails.metadata.invocationId` identifies the run, and is the same for all
provenance one deploy writes. In order of precedence it is:

1. `--invocation-id` or `$RULES_IMG_PROVENANCE_INVOCATION_ID`.
2. The ID of the Bazel invocation: `bazel run` exports it as `$BUILD_ID`, the ID
   Bazel prints as `Invocation ID` and uses in the build event stream and in remote
   build UIs, so the provenance can be traced to the build that published the image.
3. On GitHub Actions, the URL of the run
   (`server_url/repository/actions/runs/<id>/attempts/<n>`), when the deploy is not
   run by `bazel run`.
4. An ID that `img deploy` generates for the deploy.

`runDetails.metadata.startedOn` and `runDetails.metadata.finishedOn` (RFC 3339, UTC)
are included only when requested with `--record-timestamps`, `--started-on` or
`$SOURCE_DATE_EPOCH`, so that the default provenance of the same inputs is
byte-for-byte reproducible. `startedOn` is when the deploy started and `finishedOn`
when the provenance was written.

### `runDetails.byproducts`

Not used.

## Example

The provenance of an image published from a GitHub Actions run, written with
`--definitions=bazel --reproducible --record-timestamps` and the request protocol
(`protocol = 2`). A test keeps this example in sync with the plugin.

<!-- example:start -->
```json
{
  "_type": "https://in-toto.io/Statement/v1",
  "predicate": {
    "buildDefinition": {
      "buildType": "https://github.com/bazel-contrib/rules_img/blob/main/modules/rules_img_signer_provenance/slsa-definitions.md#bazel-v1",
      "externalParameters": {
        "parameters": {
          "config": "sign"
        },
        "targets": [
          "//services/img:push"
        ],
        "workspace": {
          "path": "modules/rules_img_services",
          "ref": "refs/heads/main",
          "repository": "https://github.com/bazel-contrib/rules_img"
        }
      },
      "internalParameters": {
        "builderPlatform": "linux/amd64",
        "ci": {
          "actorId": "1951866",
          "eventName": "push",
          "refProtected": "true",
          "refType": "branch",
          "repositoryId": "286865579",
          "repositoryOwnerId": "1234",
          "runner": {
            "arch": "X64",
            "environment": "github-hosted",
            "imageOs": "ubuntu24",
            "imageVersion": "20250929.60.1",
            "os": "Linux"
          },
          "system": "github-actions"
        },
        "pushStrategy": "eager",
        "reproducible": true,
        "targetPlatform": "linux/arm64"
      },
      "resolvedDependencies": [
        {
          "digest": {
            "gitCommit": "55f1937b77c46a5dfb8cc4f8c4938b8e729c05a1"
          },
          "name": "source",
          "uri": "git+https://github.com/bazel-contrib/rules_img@refs/heads/main"
        }
      ]
    },
    "runDetails": {
      "builder": {
        "id": "https://github.com/bazel-contrib/rules_img/.github/workflows/push_services.yaml@refs/heads/main",
        "version": {
          "rules_img": "0.3.25"
        }
      },
      "metadata": {
        "finishedOn": "2023-11-14T22:15:00Z",
        "invocationId": "https://github.com/bazel-contrib/rules_img/actions/runs/18715517508/attempts/1",
        "startedOn": "2023-11-14T22:13:20Z"
      }
    }
  },
  "predicateType": "https://slsa.dev/provenance/v1",
  "subject": [
    {
      "digest": {
        "sha256": "5d3f6e0cc0a0c4e5ba9a2b7aa1a3b1ae2d3b2ab3ba0b1ad10cd8b5b4a6a8c7aa"
      },
      "name": "pkg:docker/ghcr.io/bazel-contrib/rules_img/img@1.2.3?platform=linux%2Farm64"
    }
  ]
}
```
<!-- example:end -->

## How this relates to other build types

| | `buildType` | Requested source | Resolved source |
| --- | --- | --- | --- |
| [GitHub Actions](https://github.com/slsa-framework/github-actions-buildtypes/blob/main/workflow/v1/README.md) | `https://slsa-framework.github.io/github-actions-buildtypes/workflow/v1` | `externalParameters.workflow` (`repository`, `ref`, `path`) | `git+<repository>@<ref>` with `gitCommit` |
| Google Cloud Build | `https://cloud.google.com/build/gcb-buildtypes/google-worker/v1` | `externalParameters.buildConfigSource` (`repository`, `ref`, `path`) | `git+<repository>@<ref>` with `gitCommit` |
| [kpack](https://github.com/buildpacks-community/kpack/blob/main/docs/slsa_build.md) | `https://github.com/buildpacks-community/kpack/blob/$RELEASE/docs/slsa.md` | `externalParameters.source` | `name: source`, with `sha1` for git |
| [BuildKit](https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-definitions.md) | `https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-definitions.md` | `externalParameters.configSource` | `uri` with `sha1` |
| Bazel (this document) | `…/slsa-definitions.md#bazel-v1` | `externalParameters.workspace` (`repository`, `ref`, `path`) | `name: source`, `git+<repository>@<ref>` with `gitCommit` |

The `bazel` definitions follow the conventions the others agree on: the requested
source (repository, ref, path) is an external parameter and the resolved commit
is a dependency with a `gitCommit` digest named by its SPDX download location;
what the user chooses is external, what the builder decides is internal; and the
`invocationId` is a URL that finds the run. Where they differ, it is because Bazel
has no counterpart: there are no substitutions or workflow inputs, so
`parameters` is free-form, and the Bazel `targets` replace the image or Dockerfile.

## Version history

### bazel-v1

Initial version.
