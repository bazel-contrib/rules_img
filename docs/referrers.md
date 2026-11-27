# Referrers of Pulled Images

A pulled base image can come with its *referrers*: artifacts such as SBOMs, signatures and
attestations that refer to the image through the `subject` field of their own manifest (see the
[OCI image spec's guidelines for artifact usage](https://github.com/opencontainers/image-spec/blob/main/manifest.md#guidelines-for-artifact-usage)).
`rules_img` fetches them along with the image and exposes them through the
[`PulledReferrersInfo`](#providers) provider of the pulled image, so later rules (for example, an aspect
walking from a final image up to its base) can find and verify them.

Referrers come from four sources, which can be combined:

- **Pinned digests** of trusted referrers, fetched from the image's registry.
- **Discovery by artifact type**, for artifacts whose digests can't be known in advance, such as
  every signature of an image.
- **Cosign's tag scheme**, for the signatures and attestations cosign pushes under tags named after
  the image's digest (`sha256-<hex>.sig`, `sha256-<hex>.att`) rather than as referrers.
- **Artifact specs**, for artifacts that come from somewhere other than an OCI registry, such as an
  SBOM database behind an HTTP endpoint.

Every referrer is attributed to the manifest named by its own `subject`: the pulled manifest or
index, or one of the child manifests of a pulled index. Fetching fails if a referrer has no
subject, or if its subject is not part of the pulled image. Only image manifests are supported as
referrers (not indexes): a pinned index fails the fetch, and a discovered one (see
[below](#discovery-by-artifact-type)) is skipped. Cosign's tag-scheme artifacts have no subject; they are attributed to the
manifest their tag is named after (see [below](#cosigns-tag-scheme)).

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
image, whatever it is, its [cosign tag-scheme artifacts](#cosigns-tag-scheme) too, and the
[BuildKit attestations](#buildkit-attestations) of its index.

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

## Cosign's tag scheme

Unless told to use OCI referrers (`--registry-referrers-mode=oci-1-1`), cosign doesn't attach
signatures and attestations as referrers. It pushes them as image manifests without a `subject`,
tagged after the digest of the image they're about: its signatures are the layers of
`sha256-<hex>.sig`, its attestations those of `sha256-<hex>.att`. Many images are published this
way; [distroless](https://github.com/GoogleContainerTools/distroless), for example, attests an SPDX
SBOM of every image (and of each of its platform manifests).

`referrer_cosign_tags` (on `pull`, `images.pull` and `images.referrers`) pulls them, for the pulled
manifest or index and, for an index, every child manifest (for `pull`, every child that the
`platforms` filter keeps): `"sig"` for signatures, `"att"` for attestations.

```starlark
pull(
    name = "distroless",
    digest = "sha256:...",
    registry = "gcr.io",
    repository = "distroless/static-debian13",
    referrer_cosign_tags = ["att"],
)
```

Each manifest found is attributed to the manifest whose tag it was found under. The tag is only a
naming convention, so every layer is checked to name that manifest in its signed payload, as
`cosign verify` would: the `docker-manifest-digest` of a signature's simple signing payload
(`application/vnd.dev.cosign.simplesigning.v1+json`), or the subject of the in-toto statement in an
attestation's DSSE envelope (`application/vnd.dsse.envelope.v1+json`). Any other layer fails the
fetch. Signatures are not verified: that is up to the policy that consumes them.

These artifacts are exposed through `PulledReferrersInfo` like other referrers, with `cosign_tag` set to
`"sig"` or `"att"`. Their `artifact_type` is their config's media type
(`application/vnd.oci.image.config.v1+json`); the predicate type of each attestation is in the
`predicateType` entry of its layer's annotations (`layer_annotations`). With the
[SBOM toolchain](/modules/rules_img_sbom/README.md), an image built on a base with an SBOM
attestation inherits the SBOM's packages (see [automatic referrers](automatic-referrers.md)).

Like discovery by artifact type, this queries the registry at fetch time: a `pull` repository that
sets it is not reproducible, and the `images` extension records the digests found in the lockfile
facts (as the artifact types `cosign:sig` and `cosign:att`).

To keep a `pull` reproducible (so it takes part in the repository contents cache), pin the cosign
artifacts in `referrer_digests` instead:

```starlark
pull(
    name = "distroless",
    digest = "sha256:1a64f46364620465ef9e9a8f5413757dfdf22695e050bbfd7303eeaf17a59383",
    registry = "gcr.io",
    repository = "distroless/static-debian13",
    referrer_digests = [
        "sha256:979aa44e15dd651f8be4efdf83319ea47e62914abff8f9f4397db26538c052d7",  # .att of the index
        "sha256:f8945d22e9a7ba70fc282127de007a369df107f8bad8915bdebab344a3075251",  # .att of linux/amd64
    ],
)
```

Without a tag to go by, a pinned cosign artifact is attributed to the manifest of the image that
its signed payloads name (every layer must name the same one, and it must be part of the image).
Find the digests with `crane digest <repository>:sha256-<hex>.att`, or `cosign triangulate --type
attestation <repository>@sha256:<hex>` for the tag. Cosign appends to these manifests when an
image is signed or attested again, which gives them a new digest: a pinned one stays valid as long
as the registry keeps the manifest, but new signatures and attestations need new pins.

## BuildKit attestations

`docker buildx build --sbom=true --provenance=true` (and `docker/build-push-action`, which attaches
provenance by default) doesn't attach the attestations it makes as referrers either. BuildKit
stores them [in the image index](https://github.com/moby/buildkit/blob/master/docs/attestations/attestation-storage.md):
after the runnable manifests comes an attestation manifest for each of them, with the platform
`unknown/unknown` (so that nothing runs it) and the annotations

```json
"vnd.docker.reference.type": "attestation-manifest",
"vnd.docker.reference.digest": "<digest of the platform manifest it is about>"
```

Each layer of an attestation manifest is an in-toto statement (`application/vnd.in-toto+json`):
typically an SPDX SBOM (predicate type `https://spdx.dev/Document`) and SLSA provenance, each
named by the layer's `in-toto.io/predicate-type` annotation. Since BuildKit 0.31 (with OCI media
types, the default), an attestation manifest is also an OCI artifact: artifact type
`application/vnd.docker.attestation.manifest.v1+json`, with a `subject`. With `oci-artifact=false`
it has neither, and only the index ties it to its manifest.

`referrer_buildkit_attestations = True` (on `pull`, `images.pull` and `images.referrers`) pulls
them as referrers of the manifests they are about, for every child manifest of the pulled index
(for `pull`, every child that the `platforms` filter keeps), with or without a `subject`:

```starlark
pull(
    name = "app_base",
    digest = "sha256:...",  # an index built with docker buildx build --sbom=true
    platforms = ["linux/amd64", "linux/arm64"],
    registry = "ghcr.io",
    repository = "example/app-base",
    referrer_buildkit_attestations = True,
)
```

They are exposed through `PulledReferrersInfo` like other referrers: an attestation manifest's
`artifact_type` is `application/vnd.docker.attestation.manifest.v1+json` (or its config's media
type, without OCI artifacts), and the predicate type of each statement is in the
`in-toto.io/predicate-type` entry of its layer's annotations (`layer_annotations`). With the
[SBOM toolchain](/modules/rules_img_sbom/README.md), an image built on such a base inherits the
packages of its BuildKit SBOM (see [automatic referrers](automatic-referrers.md)).

The attestation manifests are listed by the index, which its digest pins, so this asks the
registry for nothing more than their content: a `pull` that sets it stays reproducible, and the
`images` extension records them in the lockfile facts (as the artifact type `buildkit:attestation`)
like other discovered referrers. A pinned attestation manifest (in `referrer_digests`) without a
`subject` is attributed by the index too.

Registries with the referrers API list BuildKit's OCI artifact attestation manifests as referrers
of their manifests too, and some images keep their attestations only there (the attestations of
[Docker Hardened Images](https://docs.docker.com/dhi/), for example, are referrers of artifact type
`application/vnd.in-toto+json`). Discover those by artifact type, or with `"*"` (which includes the
BuildKit attestations of the index as well):

```starlark
    referrer_artifact_types = [
        "application/vnd.docker.attestation.manifest.v1+json",
        "application/vnd.in-toto+json",
    ],
```

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
`artifact_type`, `annotations`), the annotations of each layer (`layer_annotations`), the
`subject` digest, `cosign_tag` (`"sig"` or `"att"` for an artifact of cosign's tag scheme, else
`None`), and `manifest`: the referrer's `ImageManifestInfo`, whose `manifest`, `config` and layer
`blob`s are always available.

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
