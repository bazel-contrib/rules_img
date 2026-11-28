"""Providers for automatic referrers: artifacts (SBOMs, ...) generated for images."""

ReferrersInfo = provider(
    doc = """\
Referrer artifacts generated automatically for an image, ready to push.

Each referrer is an OCI artifact manifest whose `subject` is the manifest it
describes. `image_push` (and `push_specs`) push them along with the image unless
pushing automatic referrers is disabled. Generators other than SBOMs can add
referrers in the same way.
""",
    fields = dict(
        referrers = """\
List of struct(manifest_info, index_info, artifact_type, subject) to push: for an image
index, the index's own referrers followed by those of every child manifest. `manifest_info`
is the ImageManifestInfo of the artifact (`index_info` is always None), `artifact_type`
its artifact type, and `subject` the ImageManifestInfo or ImageIndexInfo it refers to.

These are the referrers as generated, for any destination. What a push pushes may differ: with
a toolchain that has `for_destination` (see AutomaticReferrersToolchainInfo), every push spec
and `image_push` pushes referrers made for its own destination.""",
        destination_state = "Internal to rules_img: what is needed to make the referrers for a push destination.",
    ),
)

ImageSbomInfo = provider(
    doc = """\
The SBOMs generated for an image by the automatic referrers toolchain: one per format.

For an image index, each SBOM describes the index and refers to the SBOMs of its
child manifests (in `manifests`).
""",
    fields = dict(
        sboms = """\
Dict mapping a format (`"cyclonedx"` or `"spdx"`) to struct(format, media_type, artifact_type,
sbom, referrer): the format, the media type of the document (for example
`application/vnd.cyclonedx+json`), the artifact type of its referrer, the File containing the
document, and the ImageManifestInfo of the OCI artifact that carries it, with the image as its
subject. In the order the toolchain generated them.""",
        manifests = "For an image index, the ImageSbomInfo of each child manifest (None for a child without one), in the order of `ImageIndexInfo.manifests`. Empty for a single manifest.",
    ),
)

AutomaticReferrersToolchainInfo = provider(
    doc = """\
Contract between rules_img and an automatic referrers toolchain.

A toolchain of type `@rules_img//img:automatic_referrers_toolchain_type` returns
`platform_common.ToolchainInfo(automatic_referrers = AutomaticReferrersToolchainInfo(...))`.
When it is registered, image rules call `generate(ctx, toolchain, request)` for
every image they build (unless the image opts out), with their own `ctx`, so the
function can declare files and run actions. It returns a list of
`struct(file, media_type, artifact_type, annotations, sbom_format)`:

- `file`: File with the artifact's content (it becomes the artifact's only layer).
- `media_type`: media type of that layer.
- `artifact_type`: `artifactType` of the artifact manifest.
- `annotations`: dict of annotations of the artifact manifest.
- `sbom_format`: `"cyclonedx"` or `"spdx"` if the artifact is an SBOM of the image, else None.
  A toolchain may return one SBOM per format.

rules_img wraps each one into an OCI artifact manifest whose `subject` is the image.

`request.kind` is `"manifest"` or `"index"`. Both kinds have:

- `label`: Label of the image target.
- `name`: the image's label, for display, as `//package:name` (`@@repo//package:name`
  outside the main repository). It is not where the image is pushed, which isn't known when
  its referrers are built: the same referrers are pushed with the image to every destination.
- `stamp`: None unless the image is stamped (as its `stamp` attribute, the global stamp
  setting and `--stamp` decide), else struct(stable_status, volatile_status): Bazel's
  workspace status files (`ctx.info_file` and `ctx.version_file`). Anything a toolchain
  records that changes from build to build (a timestamp, ...) should come from these, so
  unstamped builds stay reproducible.
- `digest`: File containing the digest of the image (`sha256:...`).
- `descriptor`: File containing the image's descriptor.

A `"manifest"` request also has:

- `manifest`: ImageManifestInfo of the image.
- `metadata_graph`: File with the package metadata graph of the image's layers, in the
  graph JSON format of `@supply_chain_tools//gather_metadata` (the image is the root target).
- `metadata`: TransitiveMetadataInfo the graph was serialized from.
- `metadata_files`: depset of the package metadata files the graph refers to.
- `layers`: the image's layers, bottom (its base image's) to top, as
  struct(layer, origin, base, scan): the layer's SingleLayerInfo, `"pulled"` for a layer of a
  pulled (or imported) image or `"built"` for one built by Bazel, whether it is a layer of the
  base image, and the File `scan_layer` returned for it, or None.
- `base`: None, or struct(image, sboms) describing the base image. `image` is
  struct(repository, registries, digest) for a pulled base (its root manifest or index),
  or None. `sboms` lists the SBOMs found for the base manifest, best match first, as
  struct(file, format, encoding, media_type, artifact_type, digest, scope): `format` is
  `"cyclonedx"` or `"spdx"`, `encoding` is `"document"` (the file is the SBOM),
  `"in-toto"` (an in-toto statement whose predicate is the SBOM, possibly as a JSON
  string), `"dsse"` (a DSSE envelope of such a statement, as cosign's tag-scheme
  attestations carry it) or `"sigstore-bundle"` (a sigstore bundle with such an
  envelope), and
  `digest` is the digest of the SBOM's referrer manifest, or None for an SBOM generated
  in this build, and `scope` is `"manifest"` for an SBOM of the base manifest itself, or
  `"index"` for one of the index it was selected from (which describes every platform).

When the toolchain has a `scan_layer` function, rules_img calls it once for every layer of
every image (and of its base images), from an aspect on the layer's target, so a layer shared
by many images is scanned once. It returns a File with the result of scanning the layer, or
None to not scan it, and gets a request with:

- `label`: Label of the target that provides the layer (a layer rule, or a pulled image).
- `name`: a prefix, unique per layer of that target, for the files the function declares.
- `origin`: `"pulled"` for a layer of a pulled (or imported) image, `"built"` for one built
  by Bazel.
- `media_type`: the layer's media type (a tar, EROFS, ext4, ... layer).
- `metadata`: File with the layer's metadata (digest, diff_id, ...), or None for a plain tar
  file used as a layer.
- `layer`: the layer's SingleLayerInfo, or None for a plain tar file used as a layer.
- `contents`: None if the layer's files are not available (a pulled layer whose blob was not
  downloaded), else struct(executable, arguments, inputs): running the File `executable` with
  `arguments` (strings) writes the files of the layer to its standard output, as an
  uncompressed tar, whatever the layer's format (a compressed tar, a compact stream, an EROFS
  or ext4 filesystem image, ...). The action that runs it needs `executable` and the depset
  `inputs`.

An `"index"` request also has:

- `index`: ImageIndexInfo of the image index.
- `manifests`: list of struct(manifest, digest, sbom), one per child manifest: its
  ImageManifestInfo, a File with its digest, and its ImageSbomInfo (or None), which has
  an SBOM per format the child's toolchain generated.

When the toolchain has a `for_destination` function, rules_img calls it for every destination
an image is pushed to (each of its `push_specs`, and each `image_push` of it) that pushes its
automatic referrers, with the `ctx` of the target that pushes it. It returns the artifacts to
push to that destination instead of those `generate` returned (a list like `generate`'s, in the
same order), or None to push those. For an image index, it is first called for each child
manifest (the children's referrers are pushed into the index's repository), then for the
index, so the index's artifacts can refer to its children's. The request has:

- `kind`, `label`, `name`, `stamp`, `digest` and `descriptor`: as for `generate`, of the image
  (the child manifest, for a child of an index).
- `manifest` (for a `"manifest"`) or `index` (for an `"index"`): its ImageManifestInfo or
  ImageIndexInfo.
- `artifacts`: what `generate` returned for the image.
- `destination`: struct(configuration, destination_file): a File with the push's configuration
  (a JSON object; `registry` and `repository` say where the image is pushed, with templates
  expanded), and None or a File with the destination as `registry/repository` (a push's
  `destination_file`), which wins over the configuration's.
- `output_prefix`: a prefix, unique per image and destination, for the files the function
  declares.
- For an `"index"`, `manifests`: as for `generate`, but the ImageSbomInfo of each child
  manifest is the one `for_destination` made for the same destination.

A `bazel run` of the push can override the registry and repository (`--registry`,
`--repository`): the artifacts still describe the destination they were made for.
""",
    fields = dict(
        child_referrers = """\
What an image index does with the automatic referrers of its child manifests: `"forward"`
(the default, also when unset) publishes them with the index (pushing the index pushes
them, and its `sbom` output group has their SBOMs too), `"drop"` doesn't, when the index
has automatic referrers of its own (an index SBOM that aggregates its children's, say).
The children's own `ReferrersInfo` is unaffected: pushing a child manifest by
itself still pushes its referrers.""",
        generate = "Function `generate(ctx, toolchain, request)` returning a list of artifacts.",
        for_destination = "Optional function `for_destination(ctx, toolchain, request)` returning the artifacts to push to one destination, or None. Unset (or None) means every destination gets the artifacts `generate` returned.",
        scan_layer = "Optional function `scan_layer(ctx, toolchain, request)` returning a File with the result of scanning a layer, or None. Unset (or None) means layers are not scanned.",
        data = "Opaque data for `generate` (tools, settings), passed back as `toolchain.data`.",
    ),
)

BaseSbomInfo = provider(
    doc = "What the base image aspect found about an image's base. Internal to rules_img.",
    fields = dict(
        image_sbom = "ImageSbomInfo of a base image built with an SBOM, or None.",
        referrers = "PulledReferrersInfo of a pulled base image, or None.",
        index_info = "ImageIndexInfo of the base if it is an image index, or None.",
        pull_info = "PullInfo of a pulled base image, or None.",
    ),
)

LayerScanInfo = provider(
    doc = "Layer scans the layer scan aspect created for the layers of an image's dependencies. Internal to rules_img.",
    fields = dict(
        scans = """\
Depset of struct(keys, origin, scan), one per layer the aspect visited: `keys` is a tuple of
the Files that identify the layer (its metadata and blob, whichever it has), `origin` is
`"pulled"` or `"built"`, and `scan` the File the toolchain's `scan_layer` returned for it,
or None.""",
    ),
)
