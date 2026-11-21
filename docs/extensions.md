<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for rules_img module extensions.

<a id="images"></a>

## images

<pre>
images = use_extension("@rules_img//img:extensions.bzl", "images")
images.pull(<a href="#images.pull-name">name</a>, <a href="#images.pull-digest">digest</a>, <a href="#images.pull-layer_handling">layer_handling</a>, <a href="#images.pull-referrer_artifact_types">referrer_artifact_types</a>, <a href="#images.pull-referrer_artifacts">referrer_artifacts</a>,
            <a href="#images.pull-referrer_cosign_tags">referrer_cosign_tags</a>, <a href="#images.pull-referrer_digests">referrer_digests</a>, <a href="#images.pull-registries">registries</a>, <a href="#images.pull-registry">registry</a>, <a href="#images.pull-repository">repository</a>, <a href="#images.pull-tag">tag</a>,
            <a href="#images.pull-unsafe_allow_tag_without_digest">unsafe_allow_tag_without_digest</a>)
images.referrers(<a href="#images.referrers-manifest">manifest</a>, <a href="#images.referrers-referrer_artifact_types">referrer_artifact_types</a>, <a href="#images.referrers-referrer_artifacts">referrer_artifacts</a>, <a href="#images.referrers-referrer_cosign_tags">referrer_cosign_tags</a>,
                 <a href="#images.referrers-referrer_digests">referrer_digests</a>)
images.settings(<a href="#images.settings-config">config</a>, <a href="#images.settings-credential_helper">credential_helper</a>, <a href="#images.settings-docker_config_path">docker_config_path</a>, <a href="#images.settings-downloader">downloader</a>, <a href="#images.settings-hub_repo">hub_repo</a>, <a href="#images.settings-image_repos">image_repos</a>,
                <a href="#images.settings-timeout">timeout</a>)
</pre>

Module extension for pulling container images in Bzlmod projects.

This extension enables declarative pulling of container images using Bazel's module
system. Images are pulled once and shared across all modules, with automatic deduplication
of blobs for efficient storage.

Example usage in MODULE.bazel:

```starlark
images = use_extension("@rules_img//img:extensions.bzl", "images")

# Pull with friendly name
images.pull(
    name = "ubuntu",
    digest = "sha256:1e622c5f073b4f6bfad6632f2616c7f59ef256e96fe78bf6a595d1dc4376ac02",
    registry = "index.docker.io",
    repository = "library/ubuntu",
    tag = "24.04",
)

# Pull without name - use repository as identifier
images.pull(
    digest = "sha256:029d4461bd98f124e531380505ceea2072418fdf28752aa73b7b273ba3048903",
    registry = "gcr.io",
    repository = "distroless/base",
)

use_repo(images, "rules_img_images.bzl")
```

Access pulled images in BUILD files using the generated helper. The `name` attribute
is optional - if not specified, use the `repository` value to reference the image:

```starlark
load("@rules_img_images.bzl", "image")

image_manifest(
    name = "my_app",
    base = image("ubuntu"),  # References the friendly name
    ...
)

image_manifest(
    name = "my_other_app",
    base = image("distroless/base"),  # References the repository
    ...
)
```

The extension creates deduplicated blob repositories, so pulling multiple images
from the same base only downloads shared layers once. The `digest` parameter is
required for reproducibility.


**TAG CLASSES**

<a id="images.pull"></a>

### pull

**Attributes**

| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="images.pull-name"></a>name |  Friendly name for the image (e.g., 'ubuntu', 'distroless-base').<br><br>This name is used to reference the image in your code via the `image()` helper function. If not specified, defaults to the repository name.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | optional |  `""`  |
| <a id="images.pull-digest"></a>digest |  The image digest for reproducible pulls (e.g., "sha256:abc123...").<br><br>When specified, the image is pulled by digest instead of tag, ensuring reproducible builds. The digest must be a full SHA256 digest starting with "sha256:".   | String | optional |  `""`  |
| <a id="images.pull-layer_handling"></a>layer_handling |  Strategy for handling image layers.<br><br>This attribute controls when and how layer data is fetched from the registry.<br><br>**Available strategies:**<br><br>* **`shallow`** (default): Layer data is fetched only if needed during push operations,   but is not available during the build. This is the most efficient option for images   that are only used as base images for pushing.<br><br>* **`eager`**: Layer data is fetched in the repository rule and is always available.   This ensures layers are accessible in build actions but is inefficient as all layers   are downloaded regardless of whether they're needed. Use this for base images that   need to be read or inspected during the build.<br><br>* **`lazy`**: Layer data is downloaded in a build action when requested. This provides   access to layers during builds while avoiding unnecessary downloads, but requires   network access during the build phase. **EXPERIMENTAL:** Use at your own risk.   | String | optional |  `"shallow"`  |
| <a id="images.pull-referrer_artifact_types"></a>referrer_artifact_types |  Artifact types of referrers to discover and pull, without knowing their digests.<br><br>For the pulled manifest or index and, if that is an index, every child manifest, the referrers of these artifact types are listed and pulled like those in `referrer_digests`. Listing follows the distribution spec: the registry's referrers API (following pagination), falling back to the referrers tag schema (`sha256-<hex>`) only if the API answers `404`. Since a referrers tag is maintained by clients rather than the registry, every manifest it lists is fetched, and kept only if its own artifact type matches and its `subject` is the manifest. Discovery always uses the `img` tool, even with `downloader = "bazel"`, because Bazel's downloader exposes neither response status nor headers.<br><br>This is meant for artifacts whose digests can't be known in advance, such as every signature of an image, which a policy can then verify downstream. For example: `["application/vnd.cncf.notary.signature", "application/vnd.dev.sigstore.bundle.v0.3+json"]`. `"*"` stands for any artifact type: `["*"]` pulls every referrer, and cosign's tag-scheme signatures and attestations too (as `referrer_cosign_tags = ["sig", "att"]` does).<br><br>The discovered digests are recorded in the lockfile facts, per manifest and artifact type, so later evaluations reuse them instead of querying the registry again. To pick up new referrers, drop the `referrer_discovery@` facts of the manifest (or remove and re-add the artifact type). Without facts support, the extension is not reproducible when this is set.   | List of strings | optional |  `[]`  |
| <a id="images.pull-referrer_artifacts"></a>referrer_artifacts |  Referrer artifact spec files describing artifacts from other sources, such as an SBOM database.<br><br>Each spec is a JSON file, typically written by your own repository rule, that lists artifacts and the files that make them up. When the image repository is fetched, every artifact becomes an OCI artifact manifest (an empty config, the files as layers) whose `subject` is the pulled manifest or index (or the `subject` the spec names, which must be the pulled manifest or index or one of its children), so it is exposed through `PulledReferrersInfo` like a referrer from `referrer_digests`. See [the referrers documentation](/docs/referrers.md) for the format.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="images.pull-referrer_cosign_tags"></a>referrer_cosign_tags |  Cosign tag-scheme artifacts to discover and pull: `"sig"` for signatures, `"att"` for attestations.<br><br>Unless told to use OCI referrers, cosign doesn't attach signatures and attestations as referrers: it pushes them as manifests tagged after the digest of the image, `sha256-<hex>.sig` and `sha256-<hex>.att`, without a `subject`. For the pulled manifest or index and, if that is an index, every child manifest, the manifests under these tags are pulled like referrers, and attributed to the manifest they are tagged after. As the tag is only a naming convention, every layer of such a manifest is checked to name that manifest in its signed payload: the `docker-manifest-digest` of a signature's simple signing payload, or the subject of an attestation's in-toto statement. Any other content fails the evaluation. Signatures are not verified.<br><br>The discovered digests are recorded in the lockfile facts like those of `referrer_artifact_types` (as the artifact types `cosign:sig` and `cosign:att`).   | List of strings | optional |  `[]`  |
| <a id="images.pull-referrer_digests"></a>referrer_digests |  Digests of trusted referrer manifests (SBOMs, signatures, attestations, ...) to pull.<br><br>Each referrer is fetched by digest, along with its config and layers -- regardless of `layer_handling`, since artifacts like an SBOM are meant to be read during the build. The empty config most artifacts use is never fetched.<br><br>A referrer is attributed to the manifest named by its own `subject` field, which must be the pulled manifest or index or, if that is an index, one of its child manifests. Evaluating the extension fails if a referrer has no subject, or applies to any other manifest. Only image manifests are supported as referrers. Cosign's tag-scheme signatures and attestations (see `referrer_cosign_tags`) have no subject: they are attributed to the manifest that their signed payloads name.<br><br>The referrers are exposed through the `PulledReferrersInfo` provider of every pulled image containing the manifest they apply to.   | List of strings | optional |  `[]`  |
| <a id="images.pull-registries"></a>registries |  List of mirror registries to try in order.<br><br>These registries will be tried in order before the primary registry. Useful for corporate environments with registry mirrors or air-gapped setups.   | List of strings | optional |  `[]`  |
| <a id="images.pull-registry"></a>registry |  Primary registry to pull from (e.g., "index.docker.io", "gcr.io").<br><br>If not specified, defaults to Docker Hub. Can be overridden by entries in registries list.   | String | optional |  `""`  |
| <a id="images.pull-repository"></a>repository |  The image repository within the registry (e.g., "library/ubuntu", "my-project/my-image").<br><br>For Docker Hub, official images use "library/" prefix (e.g., "library/ubuntu").   | String | required |  |
| <a id="images.pull-tag"></a>tag |  The image tag to pull (e.g., "latest", "24.04", "v1.2.3").<br><br>While optional, it's recommended to also specify a digest for reproducible builds.   | String | optional |  `""`  |
| <a id="images.pull-unsafe_allow_tag_without_digest"></a>unsafe_allow_tag_without_digest |  Resolve `tag` to a digest each time the extension is evaluated, instead of requiring `digest`.<br><br>**WARNING:** Builds then follow the tag, so they are not reproducible.<br><br>The extension reports itself as not reproducible, so Bazel re-evaluates it in each new server instead of recording the digest in the lockfile. Image and blob repositories stay keyed by digest, so they come from the repo contents cache until the tag moves. Their repository names contain the digest: reference the image through the hub's `image()`.   | Boolean | optional |  `False`  |

<a id="images.referrers"></a>

### referrers

Attaches referrers to a manifest pulled by any module.

The manifest can be a pulled manifest or index, or a child manifest of a pulled index, and may be
pulled by another module than the one declaring the referrers:

```starlark
images.referrers(
    manifest = "sha256:...",
    referrer_digests = ["sha256:..."],
    referrer_artifact_types = ["application/vnd.cncf.notary.signature"],
)
```

In the root module, it is an error if no module pulls the manifest. In other modules, the tag is
ignored then, so a module can annotate images that only some of its users pull.

**Attributes**

| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="images.referrers-manifest"></a>manifest |  Digest of the manifest or index the referrers apply to.   | String | required |  |
| <a id="images.referrers-referrer_artifact_types"></a>referrer_artifact_types |  Artifact types of referrers to discover and pull, without knowing their digests.<br><br>For `manifest` and, if that is an index, every child manifest, the referrers of these artifact types are listed and pulled like those in `referrer_digests`. Listing follows the distribution spec: the registry's referrers API (following pagination), falling back to the referrers tag schema (`sha256-<hex>`) only if the API answers `404`. Since a referrers tag is maintained by clients rather than the registry, every manifest it lists is fetched, and kept only if its own artifact type matches and its `subject` is the manifest. Discovery always uses the `img` tool, even with `downloader = "bazel"`, because Bazel's downloader exposes neither response status nor headers.<br><br>This is meant for artifacts whose digests can't be known in advance, such as every signature of an image, which a policy can then verify downstream. For example: `["application/vnd.cncf.notary.signature", "application/vnd.dev.sigstore.bundle.v0.3+json"]`. `"*"` stands for any artifact type: `["*"]` pulls every referrer, and cosign's tag-scheme signatures and attestations too (as `referrer_cosign_tags = ["sig", "att"]` does).<br><br>The discovered digests are recorded in the lockfile facts, per manifest and artifact type, so later evaluations reuse them instead of querying the registry again. To pick up new referrers, drop the `referrer_discovery@` facts of the manifest (or remove and re-add the artifact type). Without facts support, the extension is not reproducible when this is set.   | List of strings | optional |  `[]`  |
| <a id="images.referrers-referrer_artifacts"></a>referrer_artifacts |  Referrer artifact spec files describing artifacts from other sources, such as an SBOM database.<br><br>Each spec is a JSON file, typically written by your own repository rule, that lists artifacts and the files that make them up. When the image repository is fetched, every artifact becomes an OCI artifact manifest (an empty config, the files as layers) whose `subject` is `manifest` (or the `subject` the spec names, which must be `manifest` or one of its children), so it is exposed through `PulledReferrersInfo` like a referrer from `referrer_digests`. See [the referrers documentation](/docs/referrers.md) for the format.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="images.referrers-referrer_cosign_tags"></a>referrer_cosign_tags |  Cosign tag-scheme artifacts to discover and pull: `"sig"` for signatures, `"att"` for attestations.<br><br>Unless told to use OCI referrers, cosign doesn't attach signatures and attestations as referrers: it pushes them as manifests tagged after the digest of the image, `sha256-<hex>.sig` and `sha256-<hex>.att`, without a `subject`. For `manifest` and, if that is an index, every child manifest, the manifests under these tags are pulled like referrers, and attributed to the manifest they are tagged after. As the tag is only a naming convention, every layer of such a manifest is checked to name that manifest in its signed payload: the `docker-manifest-digest` of a signature's simple signing payload, or the subject of an attestation's in-toto statement. Any other content fails the evaluation. Signatures are not verified.<br><br>The discovered digests are recorded in the lockfile facts like those of `referrer_artifact_types` (as the artifact types `cosign:sig` and `cosign:att`).   | List of strings | optional |  `[]`  |
| <a id="images.referrers-referrer_digests"></a>referrer_digests |  Digests of trusted referrer manifests (SBOMs, signatures, attestations, ...) to pull.<br><br>Each referrer is fetched by digest, along with its config and layers -- regardless of `layer_handling`, since artifacts like an SBOM are meant to be read during the build. The empty config most artifacts use is never fetched.<br><br>A referrer is attributed to the manifest named by its own `subject` field, which must be `manifest` or, if that is an index, one of its child manifests. Evaluating the extension fails if a referrer has no subject, or applies to any other manifest. Only image manifests are supported as referrers. Cosign's tag-scheme signatures and attestations (see `referrer_cosign_tags`) have no subject: they are attributed to the manifest that their signed payloads name.<br><br>The referrers are exposed through the `PulledReferrersInfo` provider of every pulled image containing the manifest they apply to.   | List of strings | optional |  `[]`  |

<a id="images.settings"></a>

### settings

**Attributes**

| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="images.settings-config"></a>config |  Label of a Docker-compatible registry authentication config file (for example `//images:config.json`).<br><br>This is the label-based alternative to `docker_config_path` and mutually exclusive with it. The file is used as `$REGISTRY_AUTH_FILE` for the pull tool. With lazy layer handling, it is also declared as an input of the build actions that download layers.<br><br>**WARNING:** Because the file is an action input, it may be uploaded to a remote cache or remote executor. Do not put secrets (`auth` or `identitytoken` entries) in it. Instead, reference a credential helper, for example `{"credHelpers": {"ghcr.io": "gh"}}` or `{"credsStore": "desktop"}`.<br><br>Only honored in the root module.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="images.settings-credential_helper"></a>credential_helper |  Credential helper to use for registry authentication when the module extension runs the pull tool.<br><br>If omitted, the pull tool inherits `$IMG_CREDENTIAL_HELPER` (or `$IMG_CREDENTIAL_HELPER_OCI_REGISTRY`, which takes precedence) when present.   | String | optional |  `""`  |
| <a id="images.settings-docker_config_path"></a>docker_config_path |  Path to Docker-compatible registry authentication config.<br><br>If omitted, the pull tool inherits `$REGISTRY_AUTH_FILE` when present.   | String | optional |  `""`  |
| <a id="images.settings-downloader"></a>downloader |  The tool to use for downloading manifests and blobs if the current module is the root module.<br><br>**Available options:**<br><br>* **`img_tool`** (default): Uses the `img` tool for all downloads.<br><br>* **`bazel`**: Uses Bazel's native HTTP capabilities for downloading manifests and blobs.   | String | optional |  `"img_tool"`  |
| <a id="images.settings-hub_repo"></a>hub_repo |  Controls visibility of the hub repository @rules_img_images.bzl for image access via the images macro.<br><br>**Available options:**<br><br>* **`auto`** (default): The hub repository is made visible if named repositories cannot be mapped in the current Bazel version.                         This means you either get @rules_img_images.bzl or one repository per image.<br><br>* **`enabled`**: Always create and expose the hub repository @rules_img_images.bzl for image access via the images macro.<br><br>* **`disabled`**: Do not create the hub repository.   | String | optional |  `"auto"`  |
| <a id="images.settings-image_repos"></a>image_repos |  Controls visibility of individual image repositories for direct access. Repos internally use the naming scheme img_<digest> and are mapped via friendly names (i.e. "ubuntu") if possible.<br><br>**Available options:**<br><br>* **`auto`** (default): Individual image repositories are made visible if named repositories can be mapped in the current Bazel version.                         This means you either get one repository per image or @rules_img_images.bzl.<br><br>* **`enabled`**: Always expose individual image repositories for direct access.<br><br>* **`disabled`**: Do not expose individual image repositories.   | String | optional |  `"auto"`  |
| <a id="images.settings-timeout"></a>timeout |  Maximum duration in seconds of each download performed for the pulled images.<br><br>**Only takes effect when `downloader` is `img_tool`**, since it is applied to the `img` tool invocations run by this extension and by the repositories it creates. Bazel's own downloader cannot be given a timeout from Starlark, so this setting is ignored when `downloader` is `bazel` (scale Bazel's built-in HTTP timeouts with `--http_timeout_scaling` instead).<br><br>Defaults to `0`, which leaves the timeout unset, so Bazel's default for executions in module extensions and repository rules (600 seconds) applies. Raise it when pulling large layers over a slow connection.   | Integer | optional |  `0`  |


