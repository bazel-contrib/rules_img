# rules_img_sbom

An SBOM toolchain for [rules_img](https://github.com/bazel-contrib/rules_img): every image it
builds gets a CycloneDX and/or SPDX SBOM, pushed (and optionally signed and attested) as an OCI
referrer of the image.

```starlark
# MODULE.bazel
bazel_dep(name = "rules_img_sbom", version = "...")

register_toolchains("@rules_img_sbom//toolchains:cyclonedx")  # or :spdx, or :cyclonedx_and_spdx
```

See the [supply chain security guide](/docs/supply-chain.md) for how to configure it
(`sbom_toolchain`), get SBOMs for base images, and verify what was pushed.

## Building from source

Released versions download the SBOM writer (`img-sbom`) prebuilt and don't depend on rules_go.
Any other version (a `git_override`, a `local_path_override`) builds it from source, which needs
these overrides in your **root** `MODULE.bazel` (only the root module can declare them):

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

## Development

```shell
cd modules/rules_img_sbom
bazel test //...
bazel run //tools:gazelle  # after changing Go imports
```

The package metadata aspect lives in rules_img (`//img/private/supply_chain_tools/gather_metadata`).
The document model and writers in `internal/` build on `supplychain-go`; the parts of it that
aren't importable are copied, with links to their origin.
