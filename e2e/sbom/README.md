# SBOM and OCI Referrers Example

This directory demonstrates how to generate a Software Bill of Materials (SBOM) for a container image and attach it as an OCI referrer using `supply_chain_tools`.

## Overview

The example builds a multi-platform container image, generates a CycloneDX SBOM from package metadata, and pushes both the image and the SBOM referrer to a registry.

### [app/](./app/BUILD.bazel) - Application with Package Metadata

A simple application target annotated with `package_metadata` (PURL). The `sbom` and `cyclonedx` rules from `supply_chain_tools` generate a CycloneDX SBOM from this metadata.

### [BUILD.bazel](./BUILD.bazel) - Image, SBOM, and Push

1. **Image**: Builds a multi-platform (AMD64/ARM64) container image with an Alpine base
2. **SBOM**: Generates a CycloneDX SBOM for the image using `sbom` and `cyclonedx` rules
3. **Referrer**: Wraps the SBOM as an ORAS file layer and creates a referrer manifest with `subject` pointing to the image index
4. **Push**: Pushes the image and its SBOM referrer together using `image_push` with the `referrers` attribute

## Building and Pushing

```bash
# Build the image and SBOM
bazel build //...

# Push image with SBOM referrer to registry
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
