# rules_img_buildah

Builds container images from Dockerfiles with [buildah](https://buildah.io/), integrated with
[rules_img](https://github.com/bazel-contrib/rules_img): the result of a `buildah_dockerfile` target
is a regular rules_img image (`ImageManifestInfo`), so it composes with `image_manifest`,
`image_push`, `image_load`, and can itself serve as the base of further layers.

This lives in a separate Bazel module so that regular rules_img users do not pull in the
buildah/containers Go dependency tree.

## Why a wrapper instead of plain buildah

buildah can read and write images through push/pull into an OCI layout directory, but that export
always materializes the whole image. When the base image in a Dockerfile is large, this is wasteful:
Bazel only needs the delta. The same applies to the base image: providing it to buildah through an
OCI layout requires writing every blob to disk first.

`buildah_tool` optimizes both directions by working directly with the containers storage library:

1. Bazel downloads only the base image metadata (manifest, config, layer metadata) and hands it to
   `buildah_tool`.
2. `buildah_tool` fetches base blobs directly from the registry, bypassing the Bazel cache. If the
   blobs are already in buildah's storage, the download is skipped entirely.
3. `buildah_tool` runs the buildah build, producing the new layers.
4. On export, only the new layers are written to files and enter the Bazel cache; base layers are
   passed through by reference.

## Usage

```starlark
# MODULE.bazel
bazel_dep(name = "rules_img", version = "0.3.19")
bazel_dep(name = "rules_img_buildah", version = "0.0.1")
```

```starlark
# BUILD.bazel
load("@rules_img_buildah//:buildah.bzl", "buildah_dockerfile")

buildah_dockerfile(
    name = "image",
    dockerfile = ":Dockerfile",
    context = [":file.txt"],
    layer_count = 3,
)

buildah_dockerfile(
    name = "derived",
    base = ":image",  # or an image pulled via rules_img `pull`
    dockerfile = ":Dockerfile.derived",
    context = [":file2.txt"],
    layer_count = 3,
)
```

A Dockerfile that builds on a Bazel-provided base declares it via the `BASE_IMAGE` build arg, which
`buildah_tool` points at the imported base image:

```dockerfile
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

COPY tests/file2.txt /
```

`COPY` paths are resolved against the Bazel build context: every file listed in `context` is staged
under its workspace-relative short path.

### Attributes

- `dockerfile`: the Dockerfile to build.
- `context`: files staged into the build context under their short paths.
- `layer_count`: number of layers the build adds on top of `base` (all layers when there is no
  `base`). Must match what the Dockerfile actually produces.
- `base`: a rules_img image target (`buildah_dockerfile`, `image_manifest`, or a `pull` repository).
- `remote_cache`: registry repository used for buildah `--cache-from`/`--cache-to` layer caching.
  Defaults to `//settings:default_remote_cache`.
- `estargz`: produce zstd:chunked (estargz) layers.
- `add_hosts`: extra `host:ip` mappings passed to buildah as `--add-host`.
- `architecture`, `os`: target platform of the image (defaults `amd64`/`linux`).

### Build settings

- `--@rules_img_buildah//settings:verbose`: stream buildah output to the Bazel console.
- `--@rules_img_buildah//settings:debug`: debug-level logging of buildah and image operations.
- `--@rules_img_buildah//settings:default_remote_cache`: fallback `remote_cache` for targets that
  set none.

## Requirements and caveats

- Linux only. The build action runs unsandboxed and non-remote (`no-sandbox`, `no-remote-exec`),
  since buildah needs user namespaces and its own storage under `~/.local/share/containers` (or
  `/var/lib/containers`). The native overlay driver is strongly recommended.
- Network access from the action: buildah pulls `FROM` images referenced directly in the
  Dockerfile, and base blobs of `pull`-based images are fetched straight from the upstream
  registry.

