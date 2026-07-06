<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for testing container images.

```python
load("@rules_img//img:test.bzl", "image_structure_test")
```

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


