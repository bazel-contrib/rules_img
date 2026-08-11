# Pre-release versions

Every commit to `main` is published as a version of the `rules_img` module in a
Bazel registry served from this repository's `pages` branch, so a change can be
depended on before it is released.

## Using a pre-release

Add the registry, keeping the Bazel Central Registry as the fallback for
everything else, and depend on a version:

```
# .bazelrc
common --registry=https://bazel-contrib.github.io/rules_img --registry=https://bcr.bazel.build
```

```python
# MODULE.bazel
bazel_dep(name = "rules_img", version = "0.3.20-20260811-fa8b7de")
```

The available versions are listed at
<https://bazel-contrib.github.io/rules_img>.

`--registry` is order-sensitive: Bazel asks each registry in turn and takes the
first that has the module. Listing the Bazel Central Registry second means only
`rules_img` comes from here.

There is no separate toolchain to set up. A pre-release ships a prebuilt lockfile
that fetches the `img` tool from `ghcr.io/bazel-contrib/rules_img/img` as an OCI
blob, using anonymous access, so nothing is built from source and no registry
credentials are needed.

## Version numbers

A pre-release version is the next patch version, stamped with the date and short
commit it was built from:

```
0.3.20-20260811-fa8b7de
└─┬──┘ └──┬───┘ └──┬──┘
  │       │        └─ commit
  │       └────────── commit date
  └────────────────── the next version after the last release (0.3.19)
```

The *next* version rather than the released one, because Bazel resolves a module
to the highest version anything in the graph asks for, and a pre-release sorts
below the release it belongs to. A pre-release of `0.3.19` would lose against the
released `0.3.19` the moment any other module in the graph depended on it, and the
version you asked for would silently not be the one you got. `0.3.20-…` sorts
above `0.3.19` and below `0.3.20`, so it wins until `0.3.20` is released.

Nothing here is a supported release: no version is ever removed, but a version may
be replaced if the build that produced it is re-run.

## How it is published

1. The *Push Service Images* workflow builds the `img` tool for every release
   platform and pushes it to `ghcr.io` as an ORAS artifact: one layer per
   platform, plus a `prebuilt_lockfile.json` layer naming those layers by digest
   (`//services/img` in `modules/rules_img_services`).
2. The `publish-dev-registry` job then fetches that lockfile back out of the
   artifact with the ORAS CLI — the tag names an index holding both the container
   images and the artifact, so it descends into the manifest whose `artifactType`
   matches before looking for the layer — and builds the module's source archive
   with
   `--//img/private/release:prebuilt_lockfile_override=//:prebuilt_lockfile.json`
   — the empty lockfile the source tree carries, so the archive is *not* specific
   to the commit.
3. `@rules_img_internal_tools//release/devregistry` stores the archive, layers the
   real lockfile on top of it as the version's overlay, and merges the module
   metadata, all into a checkout of the `pages` branch, which the job commits.

The registry is a plain directory tree:

```
bazel_registry.json
index.html                                          landing page, regenerated from the metadata
modules/rules_img/metadata.json                     every version ever published
modules/rules_img/<version>/MODULE.bazel            what Bazel resolves against
modules/rules_img/<version>/source.json             archive URL, integrity, overlay, patches
modules/rules_img/<version>/overlay/                the prebuilt lockfile for this commit
modules/rules_img/<version>/patches/                sets the version in the archive's MODULE.bazel
assets/sha256/<digest>/file                         the module source, addressed by content
```

Archives are stored by content, and the one thing that differs between two
commits with identical sources — the lockfile, which names blobs by digest — is
published as an [overlay](https://bazel.build/external/registry) rather than
packaged into the archive. So a commit that does not touch the module's sources
adds a few kilobytes of metadata and reuses the archive that is already there,
instead of another copy of it. (Since the stored file is named after its digest
and has no extension, `source.json` states `archive_type` for Bazel.)

Only module source is stored here; the tool binaries live in the container
registry, and are not copied into the assets.

## Running this for your own repository

The pipeline is driven by `.github/dev-registry.json`:

| Field | Meaning |
| --- | --- |
| `module` | Name of the Bazel module being published. |
| `registry_url` | Base URL the registry is served from, i.e. what a consumer passes to `--registry`. |
| `pages_branch` | Branch the registry lives on. Created by the first run if it does not exist. |
| `asset_base_url` | Optional. Where the source archives are served from; defaults to `<registry_url>/assets`. |
| `metadata_template` | Optional. BCR-style `metadata.json` seeding a module's homepage and maintainers on first publish. Relative to this file. |
| `artifact.registry`, `artifact.repository` | The ORAS artifact holding the tool binaries. Must agree with what the push job publishes and with the `registry`/`repository` recorded in the lockfile -- the publisher refuses to publish a lockfile that points somewhere else. |
| `artifact.artifact_type` | `artifactType` of the artifact manifest. The tag it is pushed under names an index that also holds the container images, so this is how the workflow picks the artifact out of it. |
| `artifact.lockfile_title` | Layer title of the lockfile inside the artifact, and the path it is overlaid at in the module. Defaults to `prebuilt_lockfile.json`. |

Serving the branch is a one-time repository setting: enable GitHub Pages for the
`pages` branch at the `/` root.

To try the publisher without pushing anything, point it at a directory and serve
the assets from that directory:

```bash
bazel run @rules_img_internal_tools//release/devregistry -- \
  --config="${PWD}/.github/dev-registry.json" \
  --version=0.3.20-20260811-fa8b7de \
  --archive="${PWD}/bazel-bin/img/private/release/rules_img.tar.gz" \
  --lockfile="${PWD}/modules/rules_img_services/bazel-bin/services/img/prebuilt_lockfile.json" \
  --output=/tmp/registry \
  --asset-base-url=file:///tmp/registry/assets
```

A workspace with `common --registry=file:///tmp/registry` then resolves the
version as a consumer would.
