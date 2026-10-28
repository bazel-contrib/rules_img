<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Rules to convert OCI layouts and container archives into container images.

Use `image_manifest_from_oci_layout` and `image_index_from_oci_layout` to import
OCI layout directories. Use `image_manifest_from_archive` and
`image_index_from_archive` to import uncompressed Docker save or OCI layout
`.tar` archives when the layer count is known. Use `image_flatten` to squash a
single-platform image or multi-platform index, OCI layout, or `.tar` archive
into one layer per platform without requiring source layer counts at Bazel
analysis time.

<a id="image_flatten"></a>

## image_flatten

<pre>
load("@rules_img//img:convert.bzl", "image_flatten")

image_flatten(<a href="#image_flatten-name">name</a>, <a href="#image_flatten-src">src</a>, <a href="#image_flatten-architecture">architecture</a>, <a href="#image_flatten-compress">compress</a>, <a href="#image_flatten-history">history</a>, <a href="#image_flatten-image">image</a>, <a href="#image_flatten-manifests">manifests</a>, <a href="#image_flatten-os">os</a>, <a href="#image_flatten-variant">variant</a>)
</pre>

Squashes all layers of a container image or multi-platform image index into a single compressed layer per platform and preserves image configuration.

Accepts either an existing `ImageManifestInfo` or `ImageIndexInfo` target via `image`, or an external container archive or OCI layout directory via `src`. Passing `src` lets Bazel import single-platform or multi-platform Docker save and OCI layout tarballs without knowing their layer counts at analysis time.

Example (single-platform archive):

```python
load("@rules_img//img:convert.bzl", "image_flatten")

image_flatten(
    name = "external_base",
    src = ":docker_save_image.tar",
    os = "linux",
    architecture = "amd64",
)
```

Example (multi-platform archive or OCI layout):

```python
load("@rules_img//img:convert.bzl", "image_flatten")

image_flatten(
    name = "external_multiarch_base",
    src = ":multiarch_oci.tar",
    manifests = [
        "linux/amd64",
        "linux/arm64",
    ],
)
```

**Known limitations.** `image_flatten` merges layers with `mutate.Extract` from go-containerregistry. Version v0.22.1 of that library has three limits. Most images do not hit them.

- If a later layer deletes the target of a hardlink, the flattened layer keeps a hardlink to a missing file. Docker then fails to load the image with a `linkat` error. The upstream report uses `RUN rm /usr/bin/git` on Alpine as an example. See [google/go-containerregistry#977](https://github.com/google/go-containerregistry/issues/977).
- If a later layer changes the target of a hardlink, the hardlink gets the new content. In the original image, the hardlink keeps the old content. See [google/go-containerregistry#2002](https://github.com/google/go-containerregistry/issues/2002).
- The library drops relative symlinks and hardlinks that point above the image root, for example `x/link -> ../../etc/file`. It does this on purpose to block path traversal. It keeps absolute links and relative links that stay inside the root. See [google/go-containerregistry#2227](https://github.com/google/go-containerregistry/pull/2227) and [google/go-containerregistry#2279](https://github.com/google/go-containerregistry/pull/2279).

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_flatten-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_flatten-src"></a>src |  Uncompressed Docker save `.tar` archive, OCI layout `.tar` archive, or OCI layout directory to flatten. Mutually exclusive with 'image'.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_flatten-architecture"></a>architecture |  Target CPU architecture for single-platform 'src' flattening.   | String | optional |  `""`  |
| <a id="image_flatten-compress"></a>compress |  Compression algorithm for each flattened layer. When set to 'auto', uses the global default compression setting.   | String | optional |  `"auto"`  |
| <a id="image_flatten-history"></a>history |  Overrides the `created_by` history entry recorded for this layer.<br><br>By default the layer records one entry, "bazel build <label>", so two layers with identical contents but different labels have different config digests. `history = []` records no history entry, so identical contents give identical config digests regardless of label. A single-element list is recorded verbatim. The img tool records at most one history entry per layer, so longer lists are rejected.   | List of strings | optional |  `["<default bazel build history>"]`  |
| <a id="image_flatten-image"></a>image |  Image manifest or multi-platform image index target to flatten. All layer blobs must be available. Mutually exclusive with 'src'.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_flatten-manifests"></a>manifests |  Ordered list of platform specifications in 'os/architecture' or 'os/architecture/variant' format when flattening a multi-platform 'src' into an ImageIndexInfo. Example: ["linux/amd64", "linux/arm64"]   | List of strings | optional |  `[]`  |
| <a id="image_flatten-os"></a>os |  Target operating system for single-platform 'src' flattening.   | String | optional |  `""`  |
| <a id="image_flatten-variant"></a>variant |  Platform variant for single-platform 'src' flattening, for example 'v3' for amd64 or 'v8' for arm64.   | String | optional |  `""`  |


<a id="image_index_from_archive"></a>

## image_index_from_archive

<pre>
load("@rules_img//img:convert.bzl", "image_index_from_archive")

image_index_from_archive(<a href="#image_index_from_archive-name">name</a>, <a href="#image_index_from_archive-src">src</a>, <a href="#image_index_from_archive-layers">layers</a>, <a href="#image_index_from_archive-manifests">manifests</a>)
</pre>

Converts an uncompressed multi-platform OCI layout or Docker save tar archive to an image index.

Use this rule when every platform manifest in the archive has the same known layer count at Bazel analysis time. When layer counts are unknown or vary across platforms, use `image_flatten` with `manifests` instead.

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_index_from_archive-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_index_from_archive-src"></a>src |  Uncompressed multi-platform OCI layout or Docker save `.tar` archive to convert.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_index_from_archive-layers"></a>layers |  Ordered list of layer media types applied to each manifest. Use the constants from `@rules_img//img:media_types.bzl`.   | List of strings | required |  |
| <a id="image_index_from_archive-manifests"></a>manifests |  Ordered list of platform specifications in 'os/architecture' or 'os/architecture/variant' format. Example: ["linux/arm64", "linux/amd64/v3"]   | List of strings | required |  |


<a id="image_index_from_oci_layout"></a>

## image_index_from_oci_layout

<pre>
load("@rules_img//img:convert.bzl", "image_index_from_oci_layout")

image_index_from_oci_layout(<a href="#image_index_from_oci_layout-name">name</a>, <a href="#image_index_from_oci_layout-src">src</a>, <a href="#image_index_from_oci_layout-layers">layers</a>, <a href="#image_index_from_oci_layout-manifests">manifests</a>)
</pre>

Converts an OCI layout directory to an image index when you know the layer media types.

For multi-platform tar archives, use `image_index_from_archive`. If you do not know the layer count, use `image_flatten`.

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_index_from_oci_layout-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_index_from_oci_layout-src"></a>src |  The directory containing the OCI layout to convert from.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_index_from_oci_layout-layers"></a>layers |  A list of layer media types. This applies to all manifests. Use the well-defined media types in @rules_img//img:media_types.bzl.   | List of strings | required |  |
| <a id="image_index_from_oci_layout-manifests"></a>manifests |  An ordered list of platform specifications in 'os/architecture' or 'os/architecture/variant' format. Example: ["linux/arm64", "linux/amd64/v3"]   | List of strings | required |  |


<a id="image_manifest_from_archive"></a>

## image_manifest_from_archive

<pre>
load("@rules_img//img:convert.bzl", "image_manifest_from_archive")

image_manifest_from_archive(<a href="#image_manifest_from_archive-name">name</a>, <a href="#image_manifest_from_archive-src">src</a>, <a href="#image_manifest_from_archive-architecture">architecture</a>, <a href="#image_manifest_from_archive-layers">layers</a>, <a href="#image_manifest_from_archive-os">os</a>, <a href="#image_manifest_from_archive-variant">variant</a>)
</pre>

Converts an uncompressed Docker save or OCI layout tar archive to a single-platform image manifest.

Use this rule when the archive layer count is known at Bazel analysis time and you want to preserve individual layers. When the archive layer count is unknown or you want a single squashed layer, use `image_flatten` instead.

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_manifest_from_archive-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_manifest_from_archive-src"></a>src |  Uncompressed Docker save or OCI layout `.tar` archive to convert.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_manifest_from_archive-architecture"></a>architecture |  Target CPU architecture for the image manifest.   | String | required |  |
| <a id="image_manifest_from_archive-layers"></a>layers |  Ordered list of layer media types. Use the constants from `@rules_img//img:media_types.bzl`.   | List of strings | required |  |
| <a id="image_manifest_from_archive-os"></a>os |  Target operating system for the image manifest.   | String | required |  |
| <a id="image_manifest_from_archive-variant"></a>variant |  Platform variant, for example 'v3' for amd64 or 'v8' for arm64.   | String | optional |  `""`  |


<a id="image_manifest_from_oci_layout"></a>

## image_manifest_from_oci_layout

<pre>
load("@rules_img//img:convert.bzl", "image_manifest_from_oci_layout")

image_manifest_from_oci_layout(<a href="#image_manifest_from_oci_layout-name">name</a>, <a href="#image_manifest_from_oci_layout-src">src</a>, <a href="#image_manifest_from_oci_layout-architecture">architecture</a>, <a href="#image_manifest_from_oci_layout-layers">layers</a>, <a href="#image_manifest_from_oci_layout-os">os</a>, <a href="#image_manifest_from_oci_layout-variant">variant</a>)
</pre>



**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_manifest_from_oci_layout-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_manifest_from_oci_layout-src"></a>src |  The directory containing the OCI layout to convert from.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_manifest_from_oci_layout-architecture"></a>architecture |  The target architecture for the image manifest.   | String | required |  |
| <a id="image_manifest_from_oci_layout-layers"></a>layers |  A list of layer media types. Use the well-defined media types in @rules_img//img:media_types.bzl.   | List of strings | required |  |
| <a id="image_manifest_from_oci_layout-os"></a>os |  The target operating system for the image manifest.   | String | required |  |
| <a id="image_manifest_from_oci_layout-variant"></a>variant |  The platform variant (e.g., 'v3' for amd64/v3, 'v8' for arm64/v8).   | String | optional |  `""`  |


