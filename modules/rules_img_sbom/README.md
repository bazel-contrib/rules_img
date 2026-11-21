# rules_img_sbom

An [automatic referrers](/docs/automatic-referrers.md) toolchain for
[rules_img](https://github.com/bazel-contrib/rules_img) that attaches an SBOM to every image it
builds, as an OCI referrer of the image.

```starlark
# MODULE.bazel
bazel_dep(name = "rules_img_sbom", version = "...")

register_toolchains("@rules_img_sbom//toolchains:cyclonedx")  # or :spdx, or :cyclonedx_and_spdx
```

That's all: every `image_manifest` and `image_index` now has an SBOM (with
`:cyclonedx_and_spdx`, one in each format, each a referrer of its own). It's exposed as the `sbom`
output group and the `ImageSbomInfo` provider, and pushed (and optionally signed, or attested:
see [image signing](/docs/image-signing.md)) along with the image.

## What's in the SBOM

- **The image** is the primary component: a `container` with the manifest digest as its SHA-256
  hash. It has no version and no package URL: a package URL names a repository, and
  where the image will be pushed isn't known when its SBOM is built (the same SBOM is pushed with
  the image wherever it goes; and every package URL type for images needs the repository's name).
  Its name is the target's label and the image's platform, such as `//app:image (linux/amd64)`, and
  its reference (`bom-ref`) is the label and the digest.
- **Packages** come from the `package_metadata` of everything the image's layers are built from,
  gathered as `@supply_chain_tools//gather_metadata` does. Dependencies between packages follow the
  build graph.
- **The base image** is a component, and its own SBOM is incorporated (see `base_sbom`).

An image index gets an SBOM that lists its manifests, refers to their SBOMs, and contains their
packages (each once, see `index_sbom`): scanners given a multi-platform tag read the index's SBOM
only. The index is named `<label> (multi-arch)`, and each manifest is listed as the primary
component of its own SBOM, with the same name and reference, so the documents name it alike.

The documents are CycloneDX 1.6 JSON or SPDX 2.3 JSON. By default they are reproducible: an unchanged
image gets the same SBOMs, byte for byte, on any machine (and so the same referrer digests and
signatures). Their serial number and namespace are derived from the image's digest, and they record
no creation time unless you ask for one (see `timestamp`).

## Configuring

Declare your own toolchain with `sbom_toolchain`:

```starlark
load("@rules_img_sbom//sbom:toolchain.bzl", "sbom_toolchain")

sbom_toolchain(
    name = "sbom_toolchain",
    formats = ["cyclonedx", "spdx"],
    base_sbom = "reference",
    artifact_types = {"cyclonedx": "sbom/cyclone-dx"},
)

toolchain(
    name = "sbom",
    toolchain = ":sbom_toolchain",
    toolchain_type = "@rules_img//img:automatic_referrers_toolchain_type",
)
```

- `formats`: the SBOMs to generate for every image, each attached as its own referrer:
  `["cyclonedx"]` (default), `["spdx"]`, or both. With both, each SBOM builds on the base image's
  SBOM in its own format when the base has one (an SBOM of the base manifest still wins over one
  of the index it was selected from), so nothing is lost in conversion; an index's SBOM in each
  format refers to its manifests' SBOMs in the same format.
- `base_sbom`:
  - `inherit` (default) copies the base image's components (its operating system and packages)
    into the image's SBOM, converting between CycloneDX and SPDX if needed, and refers to the base
    image's SBOM. Scanners like grype read a single document and need the operating-system
    component to match distro packages, so this is what makes the base image scannable through the
    image's SBOM.
  - `reference` only refers to the base image's SBOM, without copying its contents: a CycloneDX
    `bom` external reference (BOM-Link or SPDX namespace, with hashes), or an SPDX external document
    reference with a `DESCRIBED_BY` relationship.
  - `ignore` leaves the base image's SBOM out.
- `index_sbom`: what an image index's SBOM contains besides its manifests and references to their
  SBOMs. `aggregate` (default) also copies the manifests' packages into it, because scanners read
  the SBOM of the reference they're given, and for a multi-platform tag that is the index
  (`trivy image --sbom-sources oci <tag>` reads only the index's SBOM and doesn't follow
  references). `reference` keeps it a small, reference-only document: its manifests, each with its
  digest and a `bom` reference to its SBOM (by serial number and SHA-256), and no dependency graph
  (in CycloneDX; SPDX keeps the relationships that connect its packages).
- `child_sboms`: whether the manifests of an image index publish their own SBOMs with it.
  `publish` (default) pushes them along with the index (each a referrer of its manifest), and the
  index's SBOM refers to them. `drop` publishes only the index's SBOM, which then doesn't refer to
  them: one document per image, found when scanning the multi-platform tag, but none when scanning
  a platform's digest. `drop` needs `index_sbom = "aggregate"`. Either way, a manifest pushed by
  itself still pushes its own SBOMs.
- `timestamp`: the creation time the SBOMs record.
  - `none` (default): none. CycloneDX leaves out its optional timestamp; SPDX requires one, and
    gets the Unix epoch.
  - `stamp`: the value of `stamp_key` (default `BUILD_TIMESTAMP`) in Bazel's workspace status, for
    stamped images (the image's `stamp` attribute, `--@rules_img//img/settings:stamp` and `--stamp`
    decide, as for the image itself); unstamped images record none. A stable status key (say,
    `STABLE_COMMIT_TIME` from your `--workspace_status_command`) keeps stamped SBOMs reproducible.
  - `now`: the time the SBOM was generated. Each regeneration changes the SBOM's digest; a cached
    SBOM keeps its time.

  Don't rely on `BUILD_TIMESTAMP` being fixed in unstamped builds: Bazel writes the current time to
  the volatile status either way, which is why the default records no time at all.
- `artifact_types`: the artifact type of the SBOM referrers, per format. A format defaults to its
  media type (`application/vnd.cyclonedx+json` or `application/spdx+json`). Use `sbom/cyclone-dx`
  or `sbom/spdx` to follow the Notary Project's convention, as Kyverno's examples do.

Base image SBOMs can be plain documents, in-toto statements, DSSE envelopes or sigstore bundles.
Their signatures aren't verified; that's up to the policy that consumes the image's SBOM.

## The SBOM writer

The toolchains run `img-sbom`, a Go binary. Released versions of this module download it prebuilt
for the exec platform (linux on x86_64, arm64 and s390x, macOS on x86_64 and arm64, Windows on
x86_64 and arm64): the released module has no Go sources and doesn't depend on rules_go or gazelle.

## Building from source

Any other version of this module (a `git_override`, a `local_path_override`) builds `img-sbom` from
source with rules_go. Its Go dependency `github.com/bazel-contrib/supply-chain/lib/supplychain-go`
ships BUILD files that a Go repository can't load, so add this to your **root** `MODULE.bazel`:

```starlark
go_deps = use_extension("@gazelle//:extensions.bzl", "go_deps")
go_deps.gazelle_override(
    build_file_generation = "clean",
    path = "github.com/bazel-contrib/supply-chain/lib/supplychain-go",
)
```

## Development

```shell
cd modules/rules_img_sbom
bazel test //...
bazel run //tools:gazelle  # after changing Go imports
```

The package metadata aspect lives in rules_img (`//img/private/supply_chain_tools/gather_metadata`)
in the shape of `@supply_chain_tools//gather_metadata`. The document model and writers in
`internal/` build on `supplychain-go`; the parts of it that aren't importable (the graph classifier
and license rendering) are copied, with links to their origin.
