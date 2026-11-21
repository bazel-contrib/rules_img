# SBOM and OCI Referrers Example

This directory demonstrates how every image gets a Software Bill of Materials (SBOM), attached as
an OCI referrer, by registering the SBOM toolchain of
[`rules_img_sbom`](../../modules/rules_img_sbom/README.md). No per-image SBOM targets are needed:
see [automatic referrers](../../docs/automatic-referrers.md).

## Overview

The example builds a multi-platform container image, and pushes it with its SBOMs (signed, when
signing is enabled).

### [MODULE.bazel](./MODULE.bazel) - SBOM Toolchain

Registers `@rules_img_sbom//toolchains:cyclonedx_and_spdx`, so every image gets a CycloneDX and an
SPDX SBOM.

### [app/](./app/BUILD.bazel) - Application with Package Metadata

A simple application target annotated with `package_metadata` (PURL), which the SBOMs list as a
package of the image.

### [BUILD.bazel](./BUILD.bazel) - Image and Push

1. **Image**: Builds a multi-platform (AMD64/ARM64) container image on
   [distroless](https://github.com/GoogleContainerTools/distroless)
   (`gcr.io/distroless/static-debian13`). Each platform manifest gets its own SBOMs, describing the
   manifest (by digest), its base image and the packages it is built from; the index gets an SBOM
   in each format that refers to them and contains their packages, so scanning the multi-platform
   tag (as Trivy does) sees them too.

   distroless attests an SPDX SBOM of every image with cosign, under the tag
   `sha256-<digest>.att` (cosign's tag scheme rather than an OCI referrer). The `pull` in
   [MODULE.bazel](./MODULE.bazel) pins those of the index and of the two platform manifests in
   `referrer_digests`, which keeps the repository reproducible (`referrer_cosign_tags = ["att"]`
   would find them by tag on every fetch instead). The image's SBOMs inherit their Debian packages
   (`base-files`, `ca-certificates`, `tzdata`, ...), which `//:sboms_contain_base_packages` checks.

   `//:image_on_buildkit_base` is the same on a base built with `docker buildx build --sbom=true`
   (`ghcr.io/crazy-max/diun`), whose SPDX SBOMs are BuildKit attestations stored in its index.
   The `pull` sets `referrer_buildkit_attestations`, so the image's SBOMs inherit the base's Alpine
   packages, which `//:sboms_contain_buildkit_base_packages` checks.
2. **Push**: `image_push` pushes the image together with all of its SBOMs, each an OCI referrer of
   the manifest it describes.
3. **Signing** (`--config=cosign` or `--config=notation`, see [.bazelrc](./.bazelrc)): the signing
   configs in [signer/](./signer/BUILD.bazel) sign the SBOMs too (`targets` includes `"referrers"`).

## Building and Pushing

```bash
# Build the image and its SBOMs
bazel build //:image --output_groups=sbom

# Push image with its SBOM referrers to a registry
bazel run //:push
```

## Inspecting the SBOM

You can check the SBOM using common tools:

```bash
trivy image --sbom-sources oci ghcr.io/malt3/supply-chain-example:latest
```

You can also use standard tooling to inspect referrers:

```bash
regctl artifact tree ghcr.io/malt3/supply-chain-example:latest
oras discover ghcr.io/malt3/supply-chain-example:latest
regctl artifact get \
  --subject ghcr.io/malt3/supply-chain-example:latest \
  --filter-artifact-type application/vnd.cyclonedx+json | jq
```
