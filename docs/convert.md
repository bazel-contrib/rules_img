<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Rules to convert OCI layout directories to container images.

Use `image_manifest_from_oci_layout` to convert an OCI layout directory
to a single-platform container image manifest, and
`image_index_from_oci_layout` to convert an OCI layout directory
to a multi-platform container image index.

<a id="image_index_from_oci_layout"></a>

## image_index_from_oci_layout

<pre>
load("@rules_img//img:convert.bzl", "image_index_from_oci_layout")

image_index_from_oci_layout(<a href="#image_index_from_oci_layout-name">name</a>, <a href="#image_index_from_oci_layout-src">src</a>, <a href="#image_index_from_oci_layout-layers">layers</a>, <a href="#image_index_from_oci_layout-manifests">manifests</a>)
</pre>



**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_index_from_oci_layout-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_index_from_oci_layout-src"></a>src |  The directory containing the OCI layout to convert from.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_index_from_oci_layout-layers"></a>layers |  A list of layer media types. This applies to all manifests. Use the well-defined media types in @rules_img//img:media_types.bzl.   | List of strings | required |  |
| <a id="image_index_from_oci_layout-manifests"></a>manifests |  An ordered list of platform specifications in 'os/architecture' format. Example: ["linux/arm64", "linux/amd64"]   | List of strings | required |  |


<a id="image_manifest_from_oci_layout"></a>

## image_manifest_from_oci_layout

<pre>
load("@rules_img//img:convert.bzl", "image_manifest_from_oci_layout")

image_manifest_from_oci_layout(<a href="#image_manifest_from_oci_layout-name">name</a>, <a href="#image_manifest_from_oci_layout-src">src</a>, <a href="#image_manifest_from_oci_layout-architecture">architecture</a>, <a href="#image_manifest_from_oci_layout-layers">layers</a>, <a href="#image_manifest_from_oci_layout-os">os</a>)
</pre>



**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_manifest_from_oci_layout-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_manifest_from_oci_layout-src"></a>src |  The directory containing the OCI layout to convert from.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_manifest_from_oci_layout-architecture"></a>architecture |  The target architecture for the image manifest.   | String | required |  |
| <a id="image_manifest_from_oci_layout-layers"></a>layers |  A list of layer media types. Use the well-defined media types in @rules_img//img:media_types.bzl.   | List of strings | required |  |
| <a id="image_manifest_from_oci_layout-os"></a>os |  The target operating system for the image manifest.   | String | required |  |


