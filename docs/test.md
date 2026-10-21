<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for testing container images.

```python
load("@rules_img//img:test.bzl", "image_size_test", "image_structure_test")
```

<a id="image_size_test"></a>

## image_size_test

<pre>
load("@rules_img//img:test.bzl", "image_size_test")

image_size_test(<a href="#image_size_test-name">name</a>, <a href="#image_size_test-image">image</a>, <a href="#image_size_test-max_duplicated_bytes">max_duplicated_bytes</a>, <a href="#image_size_test-max_wasted_bytes">max_wasted_bytes</a>)
</pre>

Budgets the wasted and duplicated bytes an image ships across its layers.

`image_size_test` replays the image's per-layer mtrees in layer order -- like
`image_structure_test`, it never materializes layer blobs -- and fails when a
budget is exceeded:

- **Wasted bytes** (`max_wasted_bytes`): file bytes shipped in a layer that a
  later layer shadows with an overwrite, a whiteout (`.wh.<name>`), or an
  opaque whiteout (`.wh..wh..opq`). They are pulled with every image but are
  unreachable in the final filesystem.
- **Duplicated bytes** (`max_duplicated_bytes`): identical file content (by
  `sha256digest`) live at multiple paths, counted once per extra copy.
  Hardlinks (`nlink > 1`) ship their content once and symlinks carry no file
  bytes, so neither counts.

Sizes are the uncompressed tar entry sizes recorded in the mtree. At least one
budget must be set. On failure the test lists the largest offenders, each
attributed to the layer that wrote the bytes and the layer that shadowed them
(with the producing target where the layer history records it).

The `image` must be a rules_img `image_manifest` or `image_index`; for an
index, every single-platform child image is checked. If any layer contributes
no mtree (e.g. a shallow base-image layer whose blob is never fetched), the
test fails with an "incomplete layer metadata" error rather than silently
passing on a subset of the image.

Example:

```python
load("@rules_img//img:test.bzl", "image_size_test")

image_size_test(
    name = "app_size_test",
    image = ":app",
    max_wasted_bytes = 10_000_000,
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_size_test-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_size_test-image"></a>image |  Image to check: a rules_img image_manifest or image_index.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_size_test-max_duplicated_bytes"></a>max_duplicated_bytes |  Budget for duplicated bytes (identical file content live at multiple paths, counted once per extra copy). -1 (the default) leaves duplicated bytes unchecked; the measured value is still reported.   | Integer | optional |  `-1`  |
| <a id="image_size_test-max_wasted_bytes"></a>max_wasted_bytes |  Budget for wasted bytes (shipped file bytes shadowed by a later layer). -1 (the default) leaves wasted bytes unchecked; the measured value is still reported.   | Integer | optional |  `-1`  |


<a id="image_structure_test"></a>

## image_structure_test

<pre>
load("@rules_img//img:test.bzl", "image_structure_test")

image_structure_test(<a href="#image_structure_test-name">name</a>, <a href="#image_structure_test-configs">configs</a>, <a href="#image_structure_test-image">image</a>)
</pre>

Validates a container image's structure using its config JSON and mtree.

`image_structure_test` is a lightweight, hermetic analogue of
[container-structure-test](https://github.com/GoogleContainerTools/container-structure-test):
it accepts the same YAML/JSON config files, but validates the image using only its
image config JSON and its mtree filesystem listing -- it never materializes the
layer blobs into the test. Because of that, checks that require a running container
or file contents are not supported and cause a clear failure (so a migrated config
tells you exactly what won't port):

- **Supported** (`metadataTest`): env / envVars, labels, entrypoint, cmd,
  exposedPorts, volumes, workdir, user -- validated against the image config JSON.
- **Supported** (`fileExistenceTests`): path, shouldExist, permissions, uid, gid,
  isExecutableBy -- validated against the image mtree.
- **Rejected**: `commandTests` (require running the container), `fileContentTests`
  (require file bytes), `licenseTests` (require scanning a running container).

The `image` may be a rules_img `image_manifest` or `image_index`, a target that
exposes prebuilt `mtree` and `oci_image_config` output groups (paired positionally
in sorted order), or an OCI image layout directory (a tree artifact, e.g. a
rules_oci `oci_image`). For a multi-platform `image_index`, every config is
validated against each single-platform image.

Example:

```python
load("@rules_img//img:test.bzl", "image_structure_test")

image_structure_test(
    name = "hello_test",
    configs = ["testdata/hello.yaml"],
    image = ":hello",
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_structure_test-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_structure_test-configs"></a>configs |  container-structure-test config files (YAML or JSON).   | <a href="https://bazel.build/concepts/labels">List of labels</a> | required |  |
| <a id="image_structure_test-image"></a>image |  Image to validate: a rules_img image_manifest/image_index, a target exposing `mtree` + `oci_image_config` output groups, or an OCI image layout directory (rules_oci).   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |


