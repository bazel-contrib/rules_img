"""The SBOM toolchain: an automatic referrers toolchain for rules_img.

Register one of `@rules_img_sbom//toolchains:cyclonedx` or
`@rules_img_sbom//toolchains:spdx` in your root module, or define your own with
`sbom_toolchain` to configure it.
"""

load("@rules_img//img:providers.bzl", "AutomaticReferrersToolchainInfo")

_FORMATS = {
    "cyclonedx": struct(media_type = "application/vnd.cyclonedx+json", extension = "cdx.json"),
    "spdx": struct(media_type = "application/spdx+json", extension = "spdx.json"),
}

def _base_dict(base, mode):
    if base == None:
        return None
    image = None
    if base.image != None:
        image = dict(repository = base.image.repository, registries = list(base.image.registries), digest = base.image.digest)
    sboms = []
    if mode != "ignore":
        sboms = [
            dict(path = sbom.file.path, format = sbom.format, encoding = sbom.encoding, digest = sbom.digest or "", scope = sbom.scope)
            for sbom in base.sboms
        ]
    return dict(image = image, sboms = sboms)

def _child_sbom(sbom_info, sbom_format):
    """The SBOM of a child manifest to refer to: the one in sbom_format, or any."""
    if sbom_info == None or not sbom_info.sboms:
        return None
    return sbom_info.sboms.get(sbom_format, sbom_info.sboms.values()[0])

def _generate(ctx, toolchain, request):
    """Implements AutomaticReferrersToolchainInfo.generate: writes the image's SBOMs."""
    return [_generate_format(ctx, toolchain.data, request, sbom_format) for sbom_format in toolchain.data.formats]

def _generate_format(ctx, data, request, sbom_format):
    inputs = [request.descriptor]
    transitive = []
    content = dict(
        kind = request.kind,
        format = sbom_format.format,
        name = request.name,
        descriptor = request.descriptor.path,
    )

    # The workspace status is only an input when it is recorded: even without
    # --stamp, Bazel writes the current time to it.
    timestamp = dict(mode = data.timestamp, key = data.stamp_key, status_files = [])
    if data.timestamp == "stamp" and request.stamp != None:
        status_files = [request.stamp.stable_status, request.stamp.volatile_status]
        inputs.extend(status_files)
        timestamp["status_files"] = [f.path for f in status_files]
    content["timestamp"] = timestamp

    if request.kind == "manifest":
        inputs.append(request.metadata_graph)
        transitive.append(request.metadata_files)
        content["graph"] = request.metadata_graph.path
        content["base_mode"] = data.base_sbom
        content["base"] = _base_dict(request.base, data.base_sbom)
        if request.base != None and data.base_sbom != "ignore":
            inputs.extend([sbom.file for sbom in request.base.sboms])
    else:
        children = []
        for manifest in request.manifests:
            child = dict(descriptor = manifest.manifest.descriptor.path)
            inputs.append(manifest.manifest.descriptor)
            child_sbom = _child_sbom(manifest.sbom, sbom_format.format)
            if child_sbom != None:
                child["sbom"] = child_sbom.sbom.path
                child["sbom_referrer_descriptor"] = child_sbom.referrer.descriptor.path
                inputs.extend([child_sbom.sbom, child_sbom.referrer.descriptor])
            children.append(child)
        content["children"] = children
        content["index_mode"] = data.index_sbom

        # The index only refers to SBOMs that are published with it.
        content["reference_children"] = data.child_sboms == "publish"

    request_file = ctx.actions.declare_file("{}.{}.sbom_request.json".format(ctx.label.name, sbom_format.extension))
    ctx.actions.write(request_file, json.encode(content))
    out = ctx.actions.declare_file("{}.{}".format(ctx.label.name, sbom_format.extension))
    ctx.actions.run(
        executable = data.tool,
        arguments = ["--request", request_file.path, "--out", out.path],
        inputs = depset(inputs + [request_file], transitive = transitive),
        outputs = [out],
        mnemonic = "ImageSbom",
        progress_message = "Writing the {} SBOM of %{{label}}".format(sbom_format.format),
    )
    return struct(
        file = out,
        media_type = sbom_format.media_type,
        artifact_type = sbom_format.artifact_type,
        annotations = {},
        sbom_format = sbom_format.format,
    )

def _sbom_toolchain_impl(ctx):
    if not ctx.attr.formats:
        fail("formats must name at least one format")
    formats = []
    for name in ctx.attr.formats:
        if name not in _FORMATS:
            fail("unknown SBOM format {}: must be one of {}".format(repr(name), ", ".join(_FORMATS.keys())))
        if name in [f.format for f in formats]:
            fail("duplicate SBOM format " + repr(name))
        formats.append(struct(
            format = name,
            media_type = _FORMATS[name].media_type,
            extension = _FORMATS[name].extension,
            artifact_type = ctx.attr.artifact_types.get(name, _FORMATS[name].media_type),
        ))
    for name in ctx.attr.artifact_types:
        if name not in ctx.attr.formats:
            fail("artifact_types names {}, which is not in formats".format(repr(name)))
    if ctx.attr.child_sboms == "drop" and ctx.attr.index_sbom == "reference":
        fail("child_sboms = \"drop\" needs index_sbom = \"aggregate\": an index SBOM that only refers to its manifests' SBOMs is no use without them")
    return [platform_common.ToolchainInfo(
        automatic_referrers = AutomaticReferrersToolchainInfo(
            child_referrers = "drop" if ctx.attr.child_sboms == "drop" else "forward",
            generate = _generate,
            data = struct(
                formats = formats,
                base_sbom = ctx.attr.base_sbom,
                index_sbom = ctx.attr.index_sbom,
                child_sboms = ctx.attr.child_sboms,
                timestamp = ctx.attr.timestamp,
                stamp_key = ctx.attr.stamp_key,
                tool = ctx.attr._tool[DefaultInfo].files_to_run,
            ),
        ),
    )]

sbom_toolchain = rule(
    implementation = _sbom_toolchain_impl,
    doc = """An automatic referrers toolchain that generates an SBOM for every image.

Wrap it in a `toolchain` of type `@rules_img//img:automatic_referrers_toolchain_type` and
register that in your root module:

```starlark
load("@rules_img_sbom//sbom:toolchain.bzl", "sbom_toolchain")

sbom_toolchain(
    name = "sbom_toolchain",
    formats = ["cyclonedx", "spdx"],
    base_sbom = "reference",
)

toolchain(
    name = "sbom",
    toolchain = ":sbom_toolchain",
    toolchain_type = "@rules_img//img:automatic_referrers_toolchain_type",
)
```

Every image gets one SBOM per format in `formats`, each attached as a referrer of its own.
For an image manifest, the SBOM describes the image (its primary component is the image,
identified by its manifest digest), the packages of its layers (from their
`package_metadata`), and its base image. For an image index, it lists the index's manifests,
refers to their own SBOMs (those in the same format, where they have one), and by default also
contains their components (see `index_sbom`).""",
    attrs = {
        "formats": attr.string_list(
            default = ["cyclonedx"],
            doc = """Formats of the SBOMs to generate for every image, each its own referrer:
`"cyclonedx"` (CycloneDX 1.6 JSON) and/or `"spdx"` (SPDX 2.3 JSON).

When a base image has SBOMs in several formats, each SBOM is built from the base image SBOM in
its own format (an SBOM of the base manifest still wins over one of the index it was selected
from), so nothing is lost in conversion.""",
        ),
        "base_sbom": attr.string(
            default = "inherit",
            values = ["inherit", "reference", "ignore"],
            doc = """How an image's SBOM incorporates the SBOM of its base image.

The base image's SBOMs are found among the referrers of a pulled base image (see `referrer_digests`
and `referrer_artifact_types` of `pull`), or is the SBOM generated for a base image built in the
same build. CycloneDX and SPDX documents are read either way, also wrapped as in-toto
attestations or sigstore bundles.

* **`inherit`** (default): copy the components (packages, the operating system, ...) and their
  dependencies into the image's SBOM, converting between formats if needed, and refer to the base
  image's SBOM. Vulnerability scanners like grype read a single document, so this lets them see the
  base image's packages.
* **`reference`**: only refer to the base image's SBOM (a CycloneDX `bom` external reference, or
  an SPDX external document reference), without copying its contents.
* **`ignore`**: leave the base image's SBOM out; the base image is still a component.""",
        ),
        "index_sbom": attr.string(
            default = "aggregate",
            values = ["aggregate", "reference"],
            doc = """What the SBOM of an image index contains, besides its manifests and references to their SBOMs.

* **`aggregate`** (default): also the components of its manifests' SBOMs (each package once,
  however many platforms ship it), with each manifest depending on its own. Scanners look up the
  SBOM of the reference they are given: for a multi-platform tag, that is the index. Trivy
  (`trivy image --sbom-sources oci`), for example, reads only the index's SBOM and doesn't follow
  references to other SBOMs, so this is what makes a multi-platform tag scannable.
* **`reference`**: nothing else. A small document, for consumers that follow the references to the
  manifests' SBOMs: each manifest with its digest and a `bom` reference to its SBOM (by serial
  number and SHA-256), and no dependency graph (in CycloneDX; SPDX keeps the relationships that
  connect its packages).""",
        ),
        "child_sboms": attr.string(
            default = "publish",
            values = ["publish", "drop"],
            doc = """Whether the manifests of an image index publish their own SBOMs along with the index.

Either way, every manifest of the index gets its SBOMs generated (an `aggregate` index SBOM is built
from them), and a manifest pushed by itself still pushes its own.

* **`publish`** (default): pushing the index pushes the SBOMs of its manifests too, each a referrer of
  its manifest, and the index's `sbom` output group has them. The index's SBOM refers to them.
* **`drop`**: only the index's SBOMs are published with it, and the index's SBOM doesn't refer to its
  manifests' (they aren't published). Scanners given a platform manifest's digest then find no SBOM
  of its own. Needs `index_sbom = "aggregate"`, so the index's SBOM has the manifests' packages.""",
        ),
        "timestamp": attr.string(
            default = "none",
            values = ["none", "stamp", "now"],
            doc = """Whether, and which, creation time the SBOMs record.

* **`none`** (default): none, so an unchanged image always gets the same SBOM, byte for byte, and
  with it the same referrer digest (and signature). CycloneDX leaves out its (optional) timestamp;
  SPDX requires a creation time, and gets the Unix epoch.
* **`stamp`**: the value of `stamp_key` in Bazel's workspace status, for images that are stamped:
  as the image's `stamp` attribute, `--@rules_img//img/settings:stamp` and `--stamp` decide, like
  the image's own stamped values. Unstamped images record none. A key of the stable status (one
  starting with `STABLE_`, such as a commit time) keeps stamped SBOMs reproducible too.
* **`now`**: the time the SBOM was generated. Not reproducible: the SBOM (and its referrer digest)
  differs whenever it is generated again, but a cached SBOM keeps the time it was generated at.""",
        ),
        "stamp_key": attr.string(
            default = "BUILD_TIMESTAMP",
            doc = """The workspace status key recorded as the creation time with `timestamp = "stamp"`.

Its value is either Unix seconds (like `BUILD_TIMESTAMP`) or an RFC 3339 time. Looked up in the
stable status first, then in the volatile status. A stamped image fails to build if the key is
missing.""",
        ),
        "artifact_types": attr.string_dict(
            doc = """Artifact type of the SBOM referrers, per format. A format defaults to its media type
(`application/vnd.cyclonedx+json` or `application/spdx+json`), which is what most tools look for.
For example, `{"cyclonedx": "sbom/cyclone-dx"}` follows the Notary Project's convention (as
Kyverno's examples do).""",
        ),
        "_tool": attr.label(
            default = Label("//:img-sbom"),
            executable = True,
            cfg = "exec",
        ),
    },
)
