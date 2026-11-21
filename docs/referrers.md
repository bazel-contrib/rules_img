# Referrers of Pulled Images

A pulled base image can come with its *referrers*: artifacts such as SBOMs, signatures and
attestations that refer to the image through the `subject` field of their own manifest (see the
[OCI image spec's guidelines for artifact usage](https://github.com/opencontainers/image-spec/blob/main/manifest.md#guidelines-for-artifact-usage)).
`rules_img` fetches them along with the image and exposes them through the
[`PulledReferrersInfo`](#providers) provider of the pulled image, so later rules (for example, an aspect
walking from a final image up to its base) can find and verify them.

Referrers come from three sources, which can be combined:

- **Pinned digests** of trusted referrers, fetched from the image's registry.
- **Discovery by artifact type**, for artifacts whose digests can't be known in advance, such as
  every signature of an image.
- **Artifact specs**, for artifacts that come from somewhere other than an OCI registry, such as an
  SBOM database behind an HTTP endpoint.

Every referrer is attributed to the manifest named by its own `subject`: the pulled manifest or
index, or one of the child manifests of a pulled index. Fetching fails if a referrer has no
subject, or if its subject is not part of the pulled image. Only image manifests are supported as
referrers (not indexes).

Referrers are always fetched in full, regardless of the image's `layer_handling`: an SBOM or a
signature is meant to be read during the build even if the image's layers are not. The empty
config most artifacts use ([`application/vnd.oci.empty.v1+json`](https://github.com/opencontainers/image-spec/blob/main/manifest.md#guidance-for-an-empty-descriptor),
`{}`) is never downloaded: its content is known, so it is written locally.

## Pinned digests

```starlark
pull(
    name = "ubuntu",
    digest = "sha256:...",
    registry = "index.docker.io",
    repository = "library/ubuntu",
    referrer_digests = [
        "sha256:...",  # SBOM of the index
        "sha256:...",  # signature of the linux/amd64 manifest
    ],
)
```

With the module extension, the same attribute exists on `images.pull`, and `images.referrers` adds
referrers to a manifest that any module pulls, including a child manifest of a pulled index:

```starlark
images = use_extension("@rules_img//img:extensions.bzl", "images")

images.pull(
    name = "ubuntu",
    digest = "sha256:...",
    registry = "index.docker.io",
    repository = "library/ubuntu",
    referrer_digests = ["sha256:..."],
)

# Annotate a manifest pulled anywhere in the module graph.
images.referrers(
    manifest = "sha256:...",
    referrer_digests = ["sha256:..."],
)
```

In the root module, `images.referrers` fails if no module pulls the manifest. In other modules, it
is ignored then, so a module can annotate images that only some of its users pull.

Tools like `oras discover` list the referrers of an image, to find the digests to pin.

## Discovery by artifact type

`referrer_artifact_types` (on `pull`, `images.pull` and `images.referrers`) pulls every referrer of
the given artifact types, without knowing their digests. It applies to the pulled manifest or index
and, for an index, every child manifest (for `pull`, every child that the `platforms` filter keeps).
This is meant for things like signatures, which a policy then verifies downstream:

```starlark
pull(
    name = "controller",
    digest = "sha256:...",
    registry = "ghcr.io",
    repository = "example/controller",
    referrer_artifact_types = [
        "application/vnd.dev.sigstore.bundle.v0.3+json",  # cosign (new bundle format)
        "application/vnd.cncf.notary.signature",  # Notation
    ],
)
```

`"*"` stands for any artifact type: `referrer_artifact_types = ["*"]` pulls every referrer of the
image, whatever it is.

Listing follows the [distribution spec](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers):

- The registry's referrers API is asked first, following `Link` pagination. With a single artifact
  type (other than `"*"`), it is passed as the `artifactType` filter; results are filtered locally either way.
- Only if the referrers API answers `404 Not Found` does the client fall back to the
  [referrers tag schema](https://github.com/opencontainers/distribution-spec/blob/main/spec.md#referrers-tag-schema):
  the tag `<alg>-<encoded>` (the algorithm truncated to 32 characters, the encoded digest to 64, and
  characters not allowed in tags replaced by `-`). Any other error fails the fetch, rather than
  silently returning no referrers.
- If the referrers tag is missing, or is not an image index, the manifest has no referrers.

A listing from the referrers API is trusted, since the registry computes it from the manifests it
stores. A referrers tag is not, since clients maintain it. In practice, its entries may lack an
`artifactType` or carry the wrong one (cosign records the config's media type), and a tag that is
merely named after a digest is not a referrers index at all. So every manifest a referrers tag lists
is fetched, and kept only if its own artifact type (its `artifactType`, or else its config's media
type) is requested and its `subject` is the manifest.

Discovery always uses the `img` tool, even with `downloader = "bazel"`, because Bazel's downloader
exposes neither response status nor headers.

What a registry lists can change at any time, so:

- A `pull` repository that discovers referrers is not reproducible: it does not take part in the
  repository contents cache.
- The `images` extension records the discovered digests in the lockfile facts, per manifest and
  artifact type (`referrer_discovery@<digest>`), and reuses them in later evaluations instead of
  querying the registry again. To pick up new referrers, delete those facts from
  `MODULE.bazel.lock`. On Bazel versions without facts, the extension is reported as not
  reproducible instead.

## Artifacts from other sources

To attach artifacts from a source that is not an OCI registry, write a *referrer artifact spec* (for
example, from your own repository rule that queries an SBOM database) and pass its label as
`referrer_artifacts` to `pull`, `images.pull` or `images.referrers`. When the image repository is
fetched, every artifact in the spec becomes an OCI artifact manifest that refers to the image, and
from then on it is a referrer like any other: content-addressed, and exposed through
`PulledReferrersInfo`.

```json
{
  "artifacts": [
    {
      "artifactType": "application/spdx+json",
      "annotations": {"org.opencontainers.image.created": "2025-01-01T00:00:00Z"},
      "files": [
        {"path": "sbom.spdx.json", "mediaType": "application/spdx+json"}
      ]
    },
    {
      "subject": "sha256:...",
      "artifactType": "application/vnd.example.scan-result",
      "files": [
        {"path": "amd64/scan.json", "mediaType": "application/json", "annotations": {"org.example.scanner": "v2"}}
      ]
    }
  ]
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `artifacts[].artifactType` | yes | The `artifactType` of the manifest. |
| `artifacts[].subject` | no | The manifest the artifact refers to. Defaults to the manifest the spec is attached to: the pulled manifest or index (`pull`, `images.pull`), or `manifest` (`images.referrers`). Must be in that manifest's tree (the manifest, or a child of an index). |
| `artifacts[].annotations` | no | Annotations of the manifest. |
| `artifacts[].files[].path` | yes | The file, relative to the directory of the spec. It must not leave that directory. |
| `artifacts[].files[].mediaType` | yes | The media type of the layer. |
| `artifacts[].files[].annotations` | no | Annotations of the layer. `org.opencontainers.image.title` defaults to the file's base name. |

Unknown fields are an error. Each manifest gets the empty config, the files as layers (in order),
or the empty descriptor as its only layer if it has no files, and a `subject` descriptor with the
subject's media type, digest and size. The files are watched, so changing one refetches the
image repository.

A repository rule that produces a spec could look like this:

```starlark
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
```

## Providers

Every pulled image provides `PulledReferrersInfo` (with no referrers unless some were requested):

- `digest`: the digest of the pulled root manifest or index.
- `manifest_digests`: the digests of the child manifests, in the order of `ImageIndexInfo.manifests`,
  so a child picked from the index can be looked up.
- `referrers`: a dict from the digest of a manifest of the pulled image to the list of its
  `ReferrerInfo`. Only manifests with referrers have an entry, so look a manifest up with
  `referrers.get(digest, [])`.

Each `ReferrerInfo` has the fields of a referrers API entry (`digest`, `media_type`, `size`,
`artifact_type`, `annotations`), the `subject` digest, and `manifest`: the referrer's
`ImageManifestInfo`, whose `manifest`, `config` and layer `blob`s are always available.

For example, to collect the cosign signatures of the `linux/amd64` manifest of a pulled index:

```starlark
load("@rules_img//img:providers.bzl", "ImageIndexInfo", "PulledReferrersInfo")

def _signatures(image):
    referrers = image[PulledReferrersInfo]
    for (position, manifest) in enumerate(image[ImageIndexInfo].manifests):
        if manifest.os == "linux" and manifest.architecture == "amd64":
            digest = referrers.manifest_digests[position]
            return [
                referrer
                for referrer in referrers.referrers.get(digest, [])
                if referrer.artifact_type == "application/vnd.dev.sigstore.bundle.v0.3+json"
            ]
    return []
```
