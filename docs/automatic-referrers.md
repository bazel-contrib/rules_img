# Automatic Referrers (SBOMs)

rules_img can attach generated artifacts, such as an SBOM, to every image it builds, as OCI
*referrers*: artifact manifests whose `subject` is the image manifest they describe. This is off
until you register an **automatic referrers toolchain**. With none registered, images are built
exactly as before.

The [`rules_img_sbom`](/modules/rules_img_sbom/README.md) module provides one that writes a
CycloneDX or SPDX SBOM for every image:

```starlark
# MODULE.bazel
bazel_dep(name = "rules_img_sbom", version = "...")

register_toolchains("@rules_img_sbom//toolchains:cyclonedx")  # or :spdx, or :cyclonedx_and_spdx
```

A single toolchain can attach several SBOMs to every image, for example one in each format
(`:cyclonedx_and_spdx`): each is a referrer of its own.

## What an image gets

For an `image_manifest`, the toolchain describes:

- the image itself as the SBOM's primary component: a `container` component with the manifest's
  digest as its SHA-256 hash, and no package URL (when the SBOM is built, where the image will be
  pushed isn't known; the SBOMs pushed with it name it there, see
  [pushed SBOMs](#pushed-sboms-name-the-image-where-it-is-pushed));
- the packages that make up its layers, from the `package_metadata` of everything the layers
  depend on (collected by an aspect with the shape of
  [`@supply_chain_tools//gather_metadata`](https://github.com/bazel-contrib/supply-chain));
- its base image, and the base image's own SBOM: copied in (`inherit`, the default) or referred to
  (`reference`). See [base image SBOMs](#base-image-sboms).

For an `image_index`, the toolchain writes an SBOM about the index that lists its manifests (each
with its `platform`) and refers to their SBOMs. The SBOMs of the manifests are kept, and pushed
along with the index.

Scanners look up the SBOM of the reference they are given, and for a multi-platform tag that is
the index: `trivy image --sbom-sources oci <tag>` reads the first SBOM among the index's referrers,
and doesn't follow references to other SBOMs. So by default, `rules_img_sbom` also copies the
packages of the manifests' SBOMs into the index's (each package once); its `index_sbom = "reference"`
option writes a small, reference-only index SBOM instead.

Each SBOM becomes an OCI artifact manifest with an empty config, the SBOM as its only layer, and the
manifest it describes as its `subject`, the shape that `oras attach`, `notation` and Kyverno use.

The SBOMs are reproducible by default: an unchanged image gets the same SBOMs, byte for byte, so its
SBOM referrers (and their signatures) keep their digests across builds and machines. They record
no creation time unless the toolchain asks for one (`rules_img_sbom`'s `timestamp`: Bazel stamping,
or the clock). A toolchain gets the workspace status files through `request.stamp` only for images
that are stamped.

## Using them

Automatic referrers are exposed in three ways:

- **Providers**: `ImageSbomInfo` (the image's SBOMs by format, `"cyclonedx"` and/or `"spdx"`, and
  for an index the `ImageSbomInfo` of each manifest) and `ReferrersInfo` (every artifact
  to push: for an index, its own and those of its manifests). Both are in
  `@rules_img//img:providers.bzl`.
- **Output group** `sbom`: the image's SBOMs for a manifest; for an index, the index's SBOMs and
  those of all its manifests.

  ```shell
  bazel build //app:image --output_groups=sbom
  ```

- **Pushes**: `image_push` and `push_specs` push automatic referrers along with the image (with an
  index, those of every manifest too, unless the toolchain drops them: see `child_sboms` of
  `rules_img_sbom`, or `child_referrers` of `AutomaticReferrersToolchainInfo`). Turn this off per target with
  `push_automatic_referrers = "disabled"`, or for the whole build with
  `--@rules_img//img/settings:push_automatic_referrers=disabled`.

### Pushed SBOMs name the image where it is pushed

An image's SBOM is built before anything knows where the image will be pushed, so it names the
image by its label. A push knows: each of an image's `push_specs` (deployed by `multi_deploy`, or
pushed at build time) and each `image_push` of it pushes SBOMs made for its own destination.
With `rules_img_sbom` (its `pushed_image_purls`, on by default), the image's primary component
then has the package URL that a pull of the image records,
`pkg:docker/<repository>@<digest>?repository_url=<registry>`, as its package URL and reference, and
its repository in place of its label in its name (`buildbarn/bb-storage (linux/amd64)`). So an image
that uses it as its base names it alike.

For an image index, the manifests' SBOMs pushed with it are made for the same destination (the
manifests are pushed into the index's repository), and so is the index's SBOM, which refers to
those. Every destination gets its own SBOMs (and with them, their own referrer digests); the SBOM is
rewritten from the one built for the image, not built again. The `sbom` output group of the target
that pushes (the image with `push_specs`, or the `image_push`) has those it pushes.

The destination is the push's registry and repository, with templates expanded (or its
`destination_file`). A `bazel run` of the push that overrides them (`--registry`, `--repository`)
pushes SBOMs that name the destination they were made for.

A toolchain other than `rules_img_sbom` does this with an optional `for_destination` function (see
[writing a toolchain](#writing-a-toolchain)).

### Signing

Automatic referrers are pushed like the `referrers` of `image_push`, so a
[`signing_config`](/docs/image-signing.md) signs them too when its `targets` include `"referrers"`
(or `img deploy --sign_targets` does):

```starlark
signing_config(
    name = "sign",
    tool = "@rules_img_signer_notation",
    targets = ["roots", "referrers"],
)
```

With `"sbom_attestations"` in its `targets`, a signing config whose plugin can attest (cosign's)
also attests each SBOM: a signed in-toto statement about the image with the SBOM as its predicate,
pushed as another referrer of the image (see [image signing](/docs/image-signing.md)). That's what
`cosign verify-attestation` and attestation-based admission policies check:

```starlark
signing_config(
    name = "sign",
    tool = "@rules_img_signer_cosign",
    targets = ["roots", "child_manifests", "sbom_attestations"],
)
```

A signed SBOM referrer is what a policy engine verifies before trusting the SBOM. For example,
Kyverno's image validating policies look up an image's referrers by artifact type, require a valid
Notary signature on them, and read the SBOM from the referrer's layer. Kyverno's examples use the
Notary Project's `sbom/cyclone-dx` artifact type; set the toolchain's `artifact_types` to match your
policy (see [`rules_img_sbom`](/modules/rules_img_sbom/README.md)).

## Opting out

Every rule that generates automatic referrers has an `automatic_referrers` attribute:

- `auto` (default): an `image_manifest` gets them, unless it describes an artifact rather than a
  container image (it sets `artifact_type`, `subject` or `config_media_type`, like a Helm chart, an
  ORAS artifact or an SBOM). An `image_index` gets them unless it has a `subject`.
- `enabled`: always.
- `disabled`: never.

```starlark
image_manifest(
    name = "chart",
    layers = [":chart_layer"],
    automatic_referrers = "disabled",
)
```

## Base image SBOMs

The image rules walk up `base` (with an aspect) to the nearest base that has an SBOM:

- a base built in the same build gets its own SBOM, which already describes *its* base;
- a pulled base's SBOMs are among its [referrers](/docs/referrers.md). Pull them by digest
  (`referrer_digests`), by artifact type (`referrer_artifact_types`, for example
  `["application/vnd.cyclonedx+json", "application/spdx+json"]`), from cosign's attestation tags
  (`referrer_cosign_tags = ["att"]`, as for distroless), from the BuildKit attestations of the
  index (`referrer_buildkit_attestations = True`, for an image built with
  `docker buildx build --sbom=true`), or from another source (`referrer_artifacts`).

Among the referrers of a pulled base, an SBOM is a referrer whose artifact type is a CycloneDX or
SPDX media type (`application/vnd.cyclonedx+json`, `application/spdx+json`, `sbom/cyclone-dx`, ...),
an in-toto attestation or sigstore bundle with a CycloneDX or SPDX predicate (as written by `cosign
attest --new-bundle-format --type cyclonedx`), a layer of a cosign attestation tag
(`sha256-<hex>.att`): a DSSE envelope whose `predicateType` annotation is a CycloneDX or SPDX
predicate type (as written by `cosign attest --type spdxjson`), or a layer of a BuildKit attestation
manifest: an in-toto statement (`application/vnd.in-toto+json`) whose `in-toto.io/predicate-type`
annotation is one (BuildKit's SBOMs are SPDX). The SBOMs of the platform manifest the image was
built on come first, then those of the index it was selected from. A toolchain reads them in that
order, and takes the first one it can read.

Signatures of base image SBOMs are not verified at build time: pull the base image's signatures
too (`referrer_artifact_types`), and verify them with your policy.

## Writing a toolchain

Any toolchain of type `@rules_img//img:automatic_referrers_toolchain_type` can generate automatic
referrers, not only SBOMs (provenance, VEX documents, scan results, ...). It returns
`platform_common.ToolchainInfo(automatic_referrers = AutomaticReferrersToolchainInfo(generate = ..., data = ...))`.
The image rules call `generate(ctx, toolchain, request)` with their own `ctx` for every image they
build, and wrap each artifact it returns into an OCI artifact manifest whose subject is the image.
See the documentation of `AutomaticReferrersToolchainInfo` in
[`img/private/providers/automatic_referrers_info.bzl`](/img/private/providers/automatic_referrers_info.bzl)
for the request and the artifacts, and `@rules_img_sbom//sbom:toolchain.bzl` for an example.

Only one toolchain of the type is selected. To combine generators, write a toolchain that runs all
of them.

A toolchain can make what a push pushes depend on its destination: if its
`AutomaticReferrersToolchainInfo` has a `for_destination(ctx, toolchain, request)` function, the
target that pushes an image (with its own `ctx`) calls it for every destination that pushes the
image's automatic referrers, with what `generate` returned for the image and the push's
configuration (its registry and repository), and pushes the artifacts it returns. For an index, it
is called for each manifest first, then for the index, with the manifests' results.

### Scanning layers

A toolchain can also scan layers: if its `AutomaticReferrersToolchainInfo` has a
`scan_layer(ctx, toolchain, request)` function, an aspect of the image rules calls it for every layer
of every image (and of its base images), at the target that provides the layer (a layer rule, a
pulled image), so a layer shared by many images is scanned once. It returns a File with the scan's
result, or None, and gets the layer's origin (`"pulled"` or `"built"`), its media type and
`SingleLayerInfo`, and `contents`: a command (`img layer-tar`) that writes the layer's files to its
standard output as an uncompressed tar, whatever the layer's format (a compressed tar, a compact
stream, an EROFS or ext4 filesystem image, ...), or None if they aren't available (a shallow pull).
A manifest's `generate` request then lists its layers, base image's first, each with the result of
its scan (`request.layers`), and the image's `layer_scans` output group has the results (an
index's, those of its manifests). `@rules_img_sbom` uses this to scan layers with syft (see its
`sources`).
