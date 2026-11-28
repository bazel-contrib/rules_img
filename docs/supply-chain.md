# Supply Chain Security: SBOMs, Attestations and Signatures

rules_img can give every image it builds a Software Bill of Materials (SBOM), push it to the
registry next to the image, and sign it there. This guide shows the whole lifecycle and how to set
it up.

![Supply chain lifecycle: pull, build, push](visuals/supply-chain-light.svg#gh-light-mode-only)
![Supply chain lifecycle: pull, build, push](visuals/supply-chain-dark.svg#gh-dark-mode-only)

1. **Pull.** A base image often publishes its own SBOM: as an OCI referrer, as a cosign
   attestation, or as a BuildKit attestation. rules_img can pull it along with the image. If the
   base image has none, you can attach one from your own source (an SBOM database, say), or scan
   the base image's layers.
2. **Build.** An aspect walks each layer's dependencies and collects their `package_metadata`
   (package URL, license, version), the way
   [`@supply_chain_tools//gather_metadata`](https://github.com/bazel-contrib/supply-chain) does.
   Optionally, every layer is also scanned with [syft](https://github.com/anchore/syft). The
   `rules_img_sbom` toolchain merges all of this with the base image's SBOM into one SBOM per image
   (and one per image index), in CycloneDX and/or SPDX.
3. **Push.** `image_push` and `multi_deploy` push the image together with its SBOMs. A
   `signing_config` signs the image and the SBOMs, and can attest each SBOM (a signed in-toto
   statement about the image). All of these are OCI referrers of the image: artifacts whose
   `subject` is the image's digest, which tools like `oras`, `cosign`, Trivy and Kyverno find by
   asking the registry "what refers to this image?".

Nothing of this happens until you register an SBOM toolchain: without one, images are built
exactly as before.

## Set up

### 1. Add the modules

```starlark
# MODULE.bazel
bazel_dep(name = "rules_img", version = "...")
bazel_dep(name = "rules_img_sbom", version = "...")           # SBOMs
bazel_dep(name = "rules_img_signer_cosign", version = "...")  # signatures and attestations

# A CycloneDX SBOM for every image. Or :spdx, or :cyclonedx_and_spdx for both.
register_toolchains("@rules_img_sbom//toolchains:cyclonedx")
```

That's enough to get SBOMs: build any image with `--output_groups=sbom` to see them, and every
push now pushes them too. The released modules ship prebuilt binaries, so they need no Go
toolchain. (Building `rules_img_sbom` from source needs a few Gazelle overrides in your root
module: see its [README](/modules/rules_img_sbom/README.md).)

To choose what goes into the SBOM, register your own toolchain instead: see
[Configure SBOM generation](#configure-sbom-generation).

### 2. Configure signing

```starlark
# BUILD.bazel
load("@rules_img//img:signing.bzl", "signing_config")

signing_config(
    name = "sign",
    tool = "@rules_img_signer_cosign",
    targets = [
        "roots",              # the pushed image or index
        "child_manifests",    # each platform manifest of an index
        "referrers",          # the SBOMs
        "sbom_attestations",  # a signed in-toto attestation of each SBOM
    ],
)
```

Turn signing on for the pushes of a release, for example in `.bazelrc`:

```
build:release --@rules_img//img/settings:sign=enabled
build:release --@rules_img//img/settings:sign_setting=//:sign
```

There are two ways to vouch for an SBOM, and you can use both:

- **`referrers`** signs the SBOM itself. It proves who pushed the SBOM. Policies that check Notary
  signatures on referrers (like Kyverno's) need this.
- **`sbom_attestations`** pushes a signed statement that says "this SBOM describes image
  `sha256:...`". It ties the SBOM to the image, which is what `cosign verify-attestation` and
  attestation-based admission policies check. Only the cosign plugin can attest; with
  `@rules_img_signer_notation`, leave it out.

Keys and tokens come from the environment `bazel run` runs in, never from the build. To choose
between cosign and Notation, and to set up keys or keyless signing, see
[image signing](image-signing.md).

### 3. Get an SBOM for your base image

An image's SBOM describes its base image too, if rules_img can find the base image's SBOM. There
are three ways, which can be combined.

#### The base image publishes an SBOM

Tell `pull` (or `images.pull`) where to find it:

| The base image publishes its SBOM as                        | Set on `pull`                                                                 |
| ----------------------------------------------------------- | ----------------------------------------------------------------------------- |
| an OCI referrer (`oras attach`, ...)                        | `referrer_digests = ["sha256:..."]`, or `referrer_artifact_types = ["application/vnd.cyclonedx+json", "application/spdx+json"]` |
| a cosign attestation (`sha256-<digest>.att` tag, e.g. distroless) | `referrer_digests = [...]`, or `referrer_cosign_tags = ["att"]`         |
| a BuildKit attestation (`docker buildx build --sbom=true`)  | `referrer_buildkit_attestations = True`                                       |

```starlark
pull(
    name = "distroless",
    digest = "sha256:1a64f46364620465ef9e9a8f5413757dfdf22695e050bbfd7303eeaf17a59383",
    registry = "gcr.io",
    repository = "distroless/static-debian13",
    referrer_digests = [
        "sha256:979aa44e15dd651f8be4efdf83319ea47e62914abff8f9f4397db26538c052d7",  # SBOM of the index
        "sha256:f8945d22e9a7ba70fc282127de007a369df107f8bad8915bdebab344a3075251",  # SBOM of linux/amd64
    ],
)
```

Prefer pinning digests (`referrer_digests`): the pull stays reproducible and takes part in the
repository contents cache. To find the digests, let rules_img look them up and pin them for you:

```shell
bazel run @rules_img//img/pull:referrers                # show what each image's registry has
bazel run @rules_img//img/pull:referrers -- --mode=apply  # add them to referrer_digests
```

`referrer_artifact_types` and `referrer_cosign_tags` instead ask the registry on every fetch (the
`images` extension records what it found in `MODULE.bazel.lock`). `referrer_buildkit_attestations`
stays reproducible, since the index lists the attestations. See
[maintaining pulled images](maintaining-pulled-images.md) for `:referrers` and the other
maintenance commands.

To add referrers to an image that another module pulls, use `images.referrers` in your root
module, with the same attributes:

```starlark
images = use_extension("@rules_img//img:extensions.bzl", "images")

images.referrers(
    manifest = "sha256:...",  # the pulled manifest or index, or one of its platform manifests
    referrer_digests = ["sha256:..."],
)
```

#### Attach SBOMs from your own source

If the SBOM of a base image lives somewhere else, such as in your own SBOM database, write a small
repository rule that downloads it, and a *referrer artifact spec* that names it:

```starlark
# sbom_db.bzl
def _sbom_db_impl(rctx):
    rctx.download(
        url = "https://sbom.example.com/api/sboms/{}".format(rctx.attr.image_digest),
        output = "sbom.spdx.json",
        sha256 = rctx.attr.sha256,
    )
    rctx.file("referrers.json", json.encode({"artifacts": [{
        "artifactType": "application/spdx+json",
        "files": [{"path": "sbom.spdx.json", "mediaType": "application/spdx+json"}],
    }]}))
    rctx.file("BUILD.bazel", 'exports_files(["referrers.json"])')

sbom_db = repository_rule(
    implementation = _sbom_db_impl,
    attrs = {"image_digest": attr.string(), "sha256": attr.string()},
)
```

Then pass the spec as `referrer_artifacts` of `pull`, `images.pull` or `images.referrers`:

```starlark
pull(
    name = "base",
    digest = "sha256:...",
    registry = "registry.example.com",
    repository = "base",
    referrer_artifacts = ["@base_sbom//:referrers.json"],
)
```

Each artifact of the spec becomes an OCI artifact whose subject is the pulled image, and from then
on it is a referrer like any pulled one. The spec's fields:

| Field                             | Required | Meaning                                                                                                             |
| --------------------------------- | -------- | ------------------------------------------------------------------------------------------------------------------- |
| `artifacts[].artifactType`        | yes      | The artifact type, such as `application/spdx+json` or `application/vnd.cyclonedx+json`.                             |
| `artifacts[].subject`             | no       | The digest of the manifest it is about. Defaults to the pulled manifest or index (or `manifest` of `images.referrers`); may be one of its platform manifests. |
| `artifacts[].annotations`         | no       | Annotations of the artifact manifest.                                                                               |
| `artifacts[].files[].path`        | yes      | The file, relative to the spec's directory (it must not leave it).                                                  |
| `artifacts[].files[].mediaType`   | yes      | The file's media type.                                                                                              |
| `artifacts[].files[].annotations` | no       | Annotations of the file's layer. `org.opencontainers.image.title` defaults to the file's name.                      |

#### Scan the base image's layers

If the base image has no SBOM at all, have its layers scanned: add `"layer_scan"` to the
toolchain's `sources` (see below), and pull the base image with `layer_handling = "eager"` (a
shallow pull doesn't download the layers, so there is nothing to scan).

## Describe your own code: `package_metadata`

Your own targets are described by their [`package_metadata`](https://github.com/bazel-contrib/supply-chain):
a package URL (purl), licenses and a version. Rulesets attach it to what they fetch (Gazelle's
`go_deps` does for every Go module, for example); for your own code, add it to the package:

```starlark
load("@package_metadata//rules:package_metadata.bzl", "package_metadata")

package(default_package_metadata = [":package_metadata"])

package_metadata(
    name = "package_metadata",
    purl = "pkg:generic/my-app@1.2.3",
)
```

Every layer's dependencies are walked, so whatever a layer is built from ends up in the SBOM, with
dependencies between packages that follow the build graph. A layer scan adds what package metadata
doesn't know about, such as the Go standard library compiled into a binary, or the packages of a
`.deb` unpacked into a layer.

## Configure SBOM generation

The toolchains in `@rules_img_sbom//toolchains` use the defaults. To change them, declare your own
`sbom_toolchain` and register it instead:

```starlark
# BUILD.bazel
load("@rules_img_sbom//sbom:toolchain.bzl", "sbom_toolchain")

sbom_toolchain(
    name = "sbom_toolchain",
    formats = ["cyclonedx", "spdx"],
    sources = ["base_sbom", "package_metadata", "layer_scan"],
)

toolchain(
    name = "sbom",
    toolchain = ":sbom_toolchain",
    toolchain_type = "@rules_img//img:automatic_referrers_toolchain_type",
)
```

```starlark
# MODULE.bazel
register_toolchains("//:sbom")
```

| Attribute            | Default                               | What it does                                                                                                                                                                                                       |
| -------------------- | ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `formats`            | `["cyclonedx"]`                       | `cyclonedx` (CycloneDX 1.6 JSON) and/or `spdx` (SPDX 2.3 JSON). Each SBOM is a referrer of its own.                                                                                                                |
| `sources`            | `["base_sbom", "package_metadata"]`   | What an SBOM is made from: the base image's SBOM, the `package_metadata` of the layers' dependencies, and/or `layer_scan` (syft). A package found by several sources is one component.                         |
| `base_sbom`          | `inherit`                             | `inherit` copies the base image's packages into the image's SBOM (scanners like grype read a single document); `reference` only links to the base image's SBOM; `ignore` leaves it out.                           |
| `scan_pulled_layers` | `best_effort`                         | With `layer_scan`: scan the layers of pulled base images if they were downloaded (`best_effort`), fail if they weren't (`required`), or don't (`disabled`).                                                      |
| `scan_built_layers`  | `enabled`                             | With `layer_scan`: scan the layers Bazel builds (`enabled`, `disabled`).                                                                                                                                           |
| `index_sbom`         | `aggregate`                           | An image index's SBOM lists its platform manifests and links to their SBOMs. `aggregate` also copies their packages in, since scanners given a multi-platform tag (like Trivy) read only the index's SBOM; `reference` doesn't. |
| `child_sboms`        | `publish`                             | `publish` pushes each platform manifest's SBOM along with the index; `drop` pushes only the index's (needs `index_sbom = "aggregate"`).                                                                          |
| `pushed_image_purls` | `enabled`                             | The SBOMs pushed with an image name it where it is pushed (`pkg:docker/<repository>@<digest>?repository_url=<registry>`), one set per destination. With `disabled`, every push pushes the same SBOMs.          |
| `timestamp`          | `none`                                | The creation time the SBOMs record: `none`, `stamp` (the workspace status key `stamp_key`, `BUILD_TIMESTAMP` by default, for stamped images), or `now`.                                                          |
| `artifact_types`     | the format's media type               | The artifact type of the SBOM referrers, per format, such as `{"cyclonedx": "sbom/cyclone-dx"}` for Kyverno's Notary Project examples.                                                                           |

By default, SBOMs are reproducible: an unchanged image gets the same SBOMs, byte for byte, on any
machine, so their referrer digests (and signatures) don't change between builds. Only
`timestamp = "now"` (or `stamp` with a volatile key) breaks that.

### Look at the SBOMs before pushing

```shell
bazel build //app:image --output_groups=sbom         # the image's SBOMs (for an index, its manifests' too)
bazel build //app:push  --output_groups=sbom         # the SBOMs exactly as this push pushes them
bazel build //app:image --output_groups=layer_scans  # what the layer scans found
```

With `pushed_image_purls` enabled (the default), the SBOMs of a push name the image where it is
pushed, so the `sbom` output group of an `image_push`, or of an image with `push_specs`, has those
SBOMs, one set per destination. Otherwise it has the image's own SBOMs.

Any SBOM tool reads them, for example `grype sbom:<path>` to check them for vulnerabilities.

### Turn it off

- For one image: `automatic_referrers = "disabled"` on `image_manifest`, `image_index` or
  `image_from_binary`. Artifacts (an `image_manifest` with `artifact_type`, `subject` or
  `config_media_type`, like a Helm chart) get no SBOM anyway.
- For one push: `push_automatic_referrers = "disabled"` on `image_push` or `image_push_spec`.
- For every push of a build: `--@rules_img//img/settings:push_automatic_referrers=disabled`.

## Push and verify

Push as usual, with signing turned on:

```shell
bazel run //app:push --config=release
```

Every referrer is attached to an image digest, so check them by digest:

```shell
IMAGE=ghcr.io/you/app
DIGEST=$(crane digest $IMAGE:latest)
```

**See what was pushed.** The image, its SBOMs, their signatures and the SBOM attestations, as a
tree of referrers:

```shell
oras discover $IMAGE@$DIGEST
regctl artifact tree $IMAGE@$DIGEST
```

**Download an SBOM:**

```shell
regctl artifact get --subject $IMAGE@$DIGEST \
  --filter-artifact-type application/vnd.cyclonedx+json > sbom.cdx.json
```

**Scan the image through its SBOM**, without pulling its layers:

```shell
trivy image --sbom-sources oci $IMAGE@$DIGEST
grype sbom:sbom.cdx.json
```

**Verify the signature** of the image (with a key; for keyless signatures, pass
`--certificate-identity` and `--certificate-oidc-issuer` instead of `--key`):

```shell
cosign verify --new-bundle-format --key cosign.pub $IMAGE@$DIGEST
```

**Verify the SBOM attestation**, and read the SBOM out of it:

```shell
cosign verify-attestation --new-bundle-format --key cosign.pub \
  --type https://cyclonedx.org/bom $IMAGE@$DIGEST \
  | jq -r .payload | base64 -d | jq .predicate
```

For the SPDX SBOM, use its versioned predicate type, `--type https://spdx.dev/Document/v2.3`. If
you signed with a key and `--tlog-upload=false`, add `--insecure-ignore-tlog=true` to both
commands. For Notation signatures, see the
[Notation plugin's README](/modules/rules_img_signer_notation/README.md#verifying).

## Reference

### How a base image's SBOM is found

The image rules walk up `base` to the nearest base image with an SBOM. A base image built in the
same build has its own generated SBOM. Among the referrers of a pulled base image, these count as
an SBOM:

- an artifact whose type is a CycloneDX or SPDX media type (`application/vnd.cyclonedx+json`,
  `application/spdx+json`, `sbom/cyclone-dx`, ...);
- an in-toto attestation or sigstore bundle with a CycloneDX or SPDX predicate (`cosign attest
  --new-bundle-format --type cyclonedx`);
- a layer of a cosign attestation tag (`sha256-<hex>.att`) whose `predicateType` is CycloneDX or
  SPDX (`cosign attest --type spdxjson`);
- a layer of a BuildKit attestation manifest whose `in-toto.io/predicate-type` is one.

The SBOMs of the platform manifest come first, then those of the index it was selected from; the
first readable one wins. With both formats configured, each SBOM builds on a base SBOM in its own
format if there is one. Signatures of base image SBOMs are not checked at build time: pull them too
(`referrer_artifact_types`) and verify them with your policy.

Cosign's tag-scheme artifacts aren't listed by the registry's referrers API, so rules_img checks
that every layer of one names the manifest its tag is named after (in its signed payload) before
attributing it. Registries without the referrers API keep their referrers in an index tagged
`sha256-<hex>`, which rules_img reads when the API answers `404`.

### Image indexes

A multi-platform image gets an SBOM per platform manifest, and one for the index, which lists the
manifests (with their platforms) and links to their SBOMs. Pushing the index pushes all of them.

### Providers and output groups

In `@rules_img//img:providers.bzl`:

- `ImageSbomInfo`: an image's SBOMs by format (`"cyclonedx"`, `"spdx"`), and for an index those of
  its manifests.
- `ReferrersInfo`: every automatic referrer to push with an image.
- `PulledReferrersInfo`: the referrers of a pulled image, by the digest of the manifest they refer
  to (`referrers.get(digest, [])`), each a `ReferrerInfo` with its artifact type, annotations, layer
  annotations, `cosign_tag` and `manifest` (an `ImageManifestInfo`, whose blobs are always
  downloaded, even for a shallow pull).

Output groups: `sbom` (on images and pushes, see [above](#look-at-the-sboms-before-pushing)) and
`layer_scans` (on images).

### Writing your own toolchain

`rules_img_sbom` is one implementation of `@rules_img//img:automatic_referrers_toolchain_type`. A
toolchain of that type can attach any artifacts to every image (provenance, VEX documents, scan
results): it returns an `AutomaticReferrersToolchainInfo` with a `generate` function, called for
every image, and optionally `scan_layer` (called once per layer) and `for_destination` (called per
push destination). See
[`automatic_referrers_info.bzl`](/img/private/providers/automatic_referrers_info.bzl) for the
contract and `@rules_img_sbom//sbom:toolchain.bzl` for an example. Only one toolchain of the type is
selected; to combine generators, write one that runs all of them.
