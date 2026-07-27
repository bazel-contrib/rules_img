# SOCI Index Manifest v2 (lazy pulling)

[SOCI](https://github.com/awslabs/soci-snapshotter) (Seekable OCI) lets a
container runtime lazily pull image layers — fetching only the files a container
actually reads — using a **ztoc** (a table of contents plus seekable-gzip
checkpoints) per layer, indexed by a **SOCI index manifest**. `rules_img` can
produce a **SOCI Index Manifest v2** for an image at build time using a pure-Go
ztoc generator (no `cgo`, no `soci` binary required).

SOCI v2 (unlike v1) does **not** use the OCI referrers API. It links a SOCI index
to its image with annotations:

- the **image manifest** gains a `com.amazon.soci.index-digest` annotation naming
  its SOCI index (this changes the image manifest's digest);
- the **SOCI index** is itself an OCI image manifest whose `layers` are the ztoc
  blobs (`application/octet-stream`) and whose config is the 2-byte `{}` blob with
  media type `application/vnd.amazon.soci.index.v2+json`;
- when the image is published as an **OCI image index**, the index gains an extra
  entry for the SOCI index (`artifactType: application/vnd.amazon.soci.index.v2+json`,
  the target platform, and a `com.amazon.soci.image-manifest-digest` annotation
  pointing back at the image manifest).

## Enabling in a Bazel build

SOCI is opt-in and off by default. Turn it on globally in your `.bazelrc` (or on
the command line):

```
common --@rules_img//img/settings:soci=enabled
```

Two tuning settings match soci-snapshotter's defaults:

```
# Uncompressed bytes between ztoc checkpoints (default 4 MiB).
common --@rules_img//img/settings:soci_span_size=4194304
# Layers smaller than this get no ztoc and are omitted from the index (default 10 MiB).
common --@rules_img//img/settings:soci_min_layer_size=10485760
```

The global `soci` flag is the default for per-layer and per-manifest `soci`
attributes (all default to `auto`), mirroring how `estargz` works:

- **`image_layer` (and other layer rules)** take a `soci` attribute
  (`auto`/`enabled`/`disabled`). When effectively enabled and the layer is
  gzip-compressed, the layer action emits a ztoc as an extra output (the `ztoc`
  output group) and records it on `SingleLayerInfo.ztoc`.
- **`image_manifest`** takes `soci`, `soci_span_size`, and `soci_min_layer_size`
  attributes. When effectively enabled it assembles the SOCI index for the image:
  it reuses each layer's ztoc when present, generates one on the fly for any
  materialized gzip layer that lacks one, and stamps the
  `com.amazon.soci.index-digest` annotation onto the image manifest.

```python
image_manifest(
    name = "app",
    base = "@distroless_cc",
    layers = [":app_layer"],
    soci = "enabled",  # or rely on the global flag
)
```

## Making lazy pulling work end-to-end

For soci-snapshotter to **discover** the SOCI index at pull time, the SOCI index
must be cross-referenced from an **OCI image index**. So wrap the image in an
`image_index` (this is also how `soci convert` bundles single-platform images):

```python
image_manifest(name = "app", layers = [":app_layer"], soci = "enabled")

image_index(
    name = "app_index",
    manifests = [":app"],
)
# app_index's index.json lists the image manifest AND its SOCI index entry
# (com.amazon.soci.image-manifest-digest), the fully discoverable v2 layout.
```

Then, on the node, run soci-snapshotter and pull the image reference published
from `app_index`; the snapshotter reads the SOCI index entry from the OCI index,
fetches the SOCI index, and lazily mounts the layers. When you push an
`image_index`, the SOCI index manifest, its `{}` config, and the ztoc blobs are
uploaded alongside the image automatically.

> **Bare `image_manifest` (no `image_index`) is annotation-only.** If you publish
> a single `image_manifest` directly, the image manifest still gets the
> `com.amazon.soci.index-digest` annotation and the SOCI index is built, but no OCI
> index / cross-reference entry is emitted and the SOCI index blobs are **not**
> pushed. That artifact is not discoverable by soci-snapshotter on its own. Wrap
> the image in an `image_index` for the discoverable, pushed layout.

## What to expect

- **Enabling SOCI changes the image manifest digest**, because the
  `com.amazon.soci.index-digest` annotation is baked into the manifest. This is
  expected and matches `soci convert`.
- **Only gzip layers get a ztoc.** zstd and uncompressed layers, empty layers, and
  non-tar artifact layers are silently omitted from the SOCI index.
- **Layers below `soci_min_layer_size` are omitted** (they don't benefit from lazy
  pulling). This filter applies to the standalone `img soci-index` CLI. When
  building through the Bazel rules, every gzip layer is currently indexed
  (regardless of size), because a layer's size is only known after its action runs
  and the ztoc blobs are shipped by position — the `soci_min_layer_size` setting is
  accepted but not yet applied on the Bazel path.
- **Shallow base-image layers** (pulled lazily, with no local blob) are skipped on
  a best-effort basis, since a ztoc cannot be generated without the layer bytes.
- The generated ztoc bytes are **byte-for-byte compatible** with soci-snapshotter's
  own ztoc format (v0.9 / zinfo v2).

## Inspecting the artifacts

```console
$ bazel build //path/to:app_index --@rules_img//img/settings:soci=enabled

# The image manifest carries the SOCI index annotation:
$ jq '.annotations' bazel-bin/path/to/app_manifest.json
{ "com.amazon.soci.index-digest": "sha256:…" }

# The SOCI index manifest lists the ztocs:
$ jq . bazel-bin/path/to/app_soci_index.json

# The OCI index cross-references the SOCI index:
$ jq '.manifests[] | select(.artifactType)' bazel-bin/path/to/app_index_index.json
```

You can also produce a ztoc or a SOCI index directly with the `img` tool:

```console
$ img ztoc --blob layer.tgz --output layer.ztoc
$ img soci-index --layer layer-metadata.json=layer.ztoc \
    --os linux --architecture amd64 \
    --manifest soci.json --config soci-config.json --descriptor soci-descriptor.json
```
