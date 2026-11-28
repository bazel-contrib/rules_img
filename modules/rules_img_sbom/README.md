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
- **Optionally, what a scan of the layers finds** (see [Scanning layers](#scanning-layers)): the
  operating system and its packages, Go modules in binaries, Python, Java and npm packages, ...

Which of these the SBOM is made from is up to you (see `sources`). A package that several describe
(a Go module in rules_go's package metadata and in the Go binary built from it, a Debian package in
the base image's SBOM and in a scan of its layers) is one component.

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

## Scanning layers

Bazel package metadata describes what Bazel built; it doesn't see what a pulled base image or a
`.deb` unpacked into a layer installed. A scan does: add `"layer_scan"` to `sources`, and every layer
of every image is scanned with [syft](https://github.com/anchore/syft), used as a library:

```starlark
sbom_toolchain(
    name = "sbom_toolchain",
    sources = ["base_sbom", "package_metadata", "layer_scan"],
    scan_pulled_layers = "best_effort",  # or "required", or "disabled"
    scan_built_layers = "enabled",  # or "disabled"
)
```

- `sources`: what the SBOM of an image manifest is made from, any combination of `base_sbom` (the
  base image's SBOM, see `base_sbom`), `package_metadata` (the Bazel package metadata of the image's
  layers) and `layer_scan`. Defaults to `["base_sbom", "package_metadata"]`.
- `scan_pulled_layers`: whether the layers of pulled base images are scanned: `best_effort`
  (default) scans those whose blobs were downloaded (a base pulled with
  `layer_handling = "shallow"` has none), `required` fails the build of an image whose base image's
  layers aren't available, and `disabled` leaves them to the base image's SBOM.
- `scan_built_layers`: whether the layers built by Bazel (`image_layer`, `layer_from_tar`, a tar
  from rules_pkg, ...) are scanned: `enabled` (default) or `disabled`.

How it works:

- **Each layer is scanned once**, by an action of the target that provides it (a layer rule, a
  pulled image), not of the image: a base image's layers are scanned once, however many images are
  built on it. Only the layers of the platforms that images are built for are scanned.
- **Every layer format**: the scan reads the layer's files as `img layer-tar` writes them, so
  compressed tars, compact streams (reassembled from their content-addressed inputs) and EROFS,
  ext4, FAT32, ISO 9660 and cpio layers are all scanned for their files. The scan is offline: it
  reads nothing but the layer (no license lookups on the network, no local Go module or Maven
  caches).
- **Layers are combined like an image's filesystem**: a package found by files that a layer above
  replaces or deletes is left out (an upgrade rewrites the package database, which then lists the
  new version), and so is a package of the base image's SBOM found that way. Operating system
  packages get the distribution of the topmost layer with an `os-release` (or of the base image's
  SBOM), even when they are installed in a layer of their own. The packages of the base image's
  layers are part of the base image's component.
- **Merged with the other sources**: a scanned package is merged into the component of the Bazel
  package metadata or base image SBOM that describes it, matched by package URL (without the
  qualifiers that only say where it came from, keeping a package's architecture and epoch). Their
  name and licenses win; CPEs and hashes are combined. rules_go's Go modules, for example, are one
  component each, and the scan adds the Go standard library (`pkg:golang/stdlib`), which no package
  metadata describes.

Each scan result is a JSON file next to the layer (`<target>_layer_scan_<n>.scan.json`), in a format
internal to this module. An image's `layer_scans` output group has those of its layers
(`bazel build //app:image --output_groups=layer_scans`).

## The SBOM writer

The toolchains run `img-sbom`, a Go binary. Released versions of this module download it prebuilt
for the exec platform (linux on x86_64, arm64 and s390x, macOS on x86_64 and arm64, Windows on
x86_64 and arm64): the released module has no Go sources and doesn't depend on rules_go or gazelle.

## Building from source

Any other version of this module (a `git_override`, a `local_path_override`) builds `img-sbom` from
source with rules_go. Some of its Go dependencies need the help of Gazelle overrides, which only
the root module can declare, so add this to your **root** `MODULE.bazel`:

```starlark
go_deps = use_extension("@gazelle//:extensions.bzl", "go_deps")

# supplychain-go ships BUILD files that a Go repository can't load.
go_deps.gazelle_override(
    build_file_generation = "clean",
    path = "github.com/bazel-contrib/supply-chain/lib/supplychain-go",
)

# syft embeds its CPE dictionary from a directory that is a Go package of its own.
go_deps.module_override(
    patch_strip = 1,
    patches = ["@rules_img_sbom//third_party:syft-cpe-index-embed.patch"],
    path = "github.com/anchore/syft",
)

# Windows builds: the checked-in generated code of cgroups' metrics.
go_deps.gazelle_override(
    directives = ["gazelle:go_generate_proto false"],
    path = "github.com/containerd/cgroups/v3",
)
```

The scanner brings syft's dependencies along (`img-sbom` is a binary of about 80 MB); rules_img's own
`img` tool doesn't depend on it.

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
