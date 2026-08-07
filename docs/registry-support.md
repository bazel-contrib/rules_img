# Registry Support Matrix

Registries differ in which parts of the [OCI distribution
spec](https://github.com/opencontainers/distribution-spec/blob/main/spec.md) they
implement, and several rules_img features are only useful — or only *work* — on a
registry that implements a particular one. The most important is **cross-repository
blob mounting**, which [deduplicated push](push-strategies.md#deduplicated-push) and
[push at build time](push-strategies.md#push-at-build-time) with a staging repository
both rest on.

This page collects what we know about specific registries. It is neither exhaustive
nor authoritative: registries change, and some entries were observed in practice
rather than read out of a vendor's documentation. If you test one of the unknowns, or
find an entry out of date, please send a pull request.

## What the columns mean

**OCI 1.1 referrers** — `GET /v2/<name>/referrers/<digest>` returns an index of the
artifacts that name that digest as their `subject`. This is how
[signatures](image-signing.md), SBOMs and other attached artifacts are discovered. A
registry without it is covered by the tag-schema fallback (the same index pushed
under the tag `sha256-<hex>`), which go-containerregistry maintains automatically on
write — so *attaching* artifacts works either way, and only discovery differs.

**Blob mount** — `POST /v2/<name>/blobs/uploads/?mount=<digest>&from=<other-name>`
answered with `201 Created`, which places a blob the registry already has under
another repository name without transferring its bytes. A registry that will not
mount answers `202 Accepted` and expects the bytes instead. The spec permits that, so
a client cannot distinguish "refuses to mount" from "does not implement mounting" —
it just uploads.

**Anonymous mount** — the same request with `mount=<digest>` but no `from`: "mount
this blob from wherever you already have it". rules_img never sends one, because
go-containerregistry only sets `mount` together with `from` (Quay rejects the bare
form). The column is here because it explains why a registry that shares blob storage
internally may still refuse a mount that does not name a source repository.

**Cross-registry mount** — a `from` repository that lives on a *different* registry,
passed as go-containerregistry's additional `origin=<registry>` parameter. Few
registries implement it, so rules_img only asks for it where a base image's recorded
source happens to be on another registry (see `cross_mount_from`). Deduplicated push
never does: its sources always name the destination's own registry, so the `origin` it
sends is that same registry.

**Automatic cross-repository sharing** — whether a blob becomes visible under *other*
repository names without being mounted at all, so that `HEAD
/v2/<other>/blobs/<digest>` returns `200` and the ordinary push skips the upload. Two
variants matter: sharing **after upload** (visible as soon as the blob is uploaded
anywhere) and sharing **after a manifest references it** (visible only once some
manifest referencing it has been created).

## The matrix

✅ supported · ❌ not supported · ❓ unknown, not tested

| Registry | OCI 1.1 referrers | Blob mount | Anonymous mount | Cross-registry mount | Automatic cross-repository sharing |
| --- | --- | --- | --- | --- | --- |
| Amazon ECR | ✅ [[1]](https://aws.amazon.com/blogs/opensource/diving-into-oci-image-and-distribution-1-1-support-in-amazon-ecr/) | ✅ [[2]](https://docs.aws.amazon.com/AmazonECR/latest/userguide/blob-mounting.html) | ❓ | ❓ | ❌ |
| Docker Hub | ❓ | ✅ [[3]](https://docs.docker.com/reference/api/registry/latest/) | ❓ | ❌ | ❌ |
| Google Artifact Registry | ✅ [[4]](https://docs.cloud.google.com/artifact-registry/docs/manage-metadata-with-attachments) | ✅ | ❓ | ✅ [[5]](https://github.com/google/go-containerregistry/issues/1321) | ❌ |
| Harbor | ✅ | ✅ | ❌ | ❓ | ❌ |
| JFrog Artifactory | ✅ | ❌ | ❌ | ❌ | ✅ after a manifest references the blob |

Notes:

- **Docker Hub** documents `mount`/`from` with a `201 Created` response, so explicit
  mounting works, and `deduplicated_push` uses it like any other registry. Its
  documented API has no `origin` parameter for naming another registry, and
  go-containerregistry deliberately strips a mount whose source registry differs from
  Docker Hub's because doing so "keeps breaking"
  ([#1741](https://github.com/google/go-containerregistry/issues/1741)) — which does
  not affect rules_img, whose mount sources always name the destination's own
  registry.
- The **referrers** column for Docker Hub is unknown, not absent: the API reference
  linked above is explicitly a subset ("It does not cover the full OCI Distribution
  Specification") and omits endpoints Docker Hub certainly implements, such as
  `/v2/<name>/tags/list`.

## Which feature needs what

| Feature | Requires |
| --- | --- |
| [`deduplicated_push`](push-strategies.md#deduplicated-push) | Blob mount |
| [`push_at_build_time_blob_repository`](push-strategies.md#push-at-build-time) (staging + cross-mount) | Blob mount |
| [`forbid_layer_push`](push-strategies.md#push-at-build-time) | Blob mount, or the blobs being present already |
| `cross_mount_from` with a base image on another registry | Cross-registry mount |
| Discovering [signatures](image-signing.md) and SBOMs with `oras discover`, `cosign`, `notation` | OCI 1.1 referrers (or a client that knows the fallback tag) |

In terms of the matrix above:

- **Amazon ECR, Google Artifact Registry, Harbor** are the interesting case for
  `deduplicated_push`: each keeps a separate blob store per repository name *and*
  mounts, so pushing K images that share their layers can upload each shared layer
  once instead of K times.
- **JFrog Artifactory**: leave `deduplicated_push` disabled. A mount is refused, and
  the strategy deliberately fails loudly rather than silently uploading into every
  repository. Its own sharing can cover part of the same ground: the first deploy of a
  new layer still pays one upload per repository (the images are pushed concurrently,
  so none of them has created a manifest yet), while a later deploy of those same
  layers may upload nothing, because the plain per-blob `HEAD` now finds them. Whether
  it does depends on the instance and on the repository paths involved, so
  [probe it](#testing-a-registry-yourself) rather than relying on it.
- **Docker Hub**: `deduplicated_push` works here too — the mounts it sends stay
  within Docker Hub, which its API documents. Note that a token scoped to a single
  repository cannot mount from another one.

Nothing here needs cross-registry mounting: `deduplicated_push` only ever mounts
between two repositories of the same registry.

## Testing a registry yourself

Push a small image to a throwaway repository (`crane copy`, or a rules_img
`image_push`), then probe with `curl`. Each mount probe that succeeds leaves the blob
in its target repository, so give every probe a fresh target name.

Getting the credentials right is most of the work, and
[`crane auth token`](https://github.com/google/go-containerregistry/blob/main/cmd/crane/doc/crane_auth_token.md)
does it for you: it reads your Docker config, exchanges it at the registry's token
service, and with `-H` prints a ready-made `Authorization:` header. It takes the
scopes a probe needs — `--push` for the repository being written to, and `--mount` for
each repository the request wants to mount *from*, which is the same pull scope
go-containerregistry requests for a cross-mount source.

```bash
REG=registry.example.com   # the registry under test
LAYER=sha256:…             # digest of a layer of the image in probe/a
MANIFEST=sha256:…          # digest of that image's manifest

# 1. OCI 1.1 referrers. 200 with an image index means supported; 404 means clients
#    need the sha256-<hex> fallback tag instead.
curl -sD- -o /dev/null -H "$(crane auth token -H "$REG/probe/a")" \
  "https://$REG/v2/probe/a/referrers/$MANIFEST"

# 2. Automatic cross-repository sharing. Run this before any mount touches probe/b,
#    or a successful mount answers 200 for the wrong reason.
#    200 -> sharing after upload. 404 -> push a manifest referencing $LAYER (any
#    repository), then repeat: a 200 now means sharing after manifest creation.
curl -sI -H "$(crane auth token -H "$REG/probe/b")" \
  "https://$REG/v2/probe/b/blobs/$LAYER"

# 3. Blob mount. 201 Created means mounted; 202 Accepted means it refused and opened
#    an upload session instead (abandon that with DELETE on the returned Location).
curl -sD- -o /dev/null -X POST \
  -H "$(crane auth token -H --push --mount "$REG/probe/a" "$REG/probe/b")" \
  "https://$REG/v2/probe/b/blobs/uploads/?mount=$LAYER&from=probe/a"

# 4. Anonymous mount: no from, fresh target repository.
curl -sD- -o /dev/null -X POST \
  -H "$(crane auth token -H --push "$REG/probe/c")" \
  "https://$REG/v2/probe/c/blobs/uploads/?mount=$LAYER"

# 5. Cross-registry mount: a source on another registry, fresh target repository.
#    No --mount here: the target registry fetches the blob itself, so what matters is
#    whether it can reach the source (usually meaning the source is public).
curl -sD- -o /dev/null -X POST \
  -H "$(crane auth token -H --push "$REG/probe/d")" \
  "https://$REG/v2/probe/d/blobs/uploads/?mount=$LAYER&from=<repo-there>&origin=<other-registry>"
```

Two things to watch out for:

- **Drop the `--mount` and step 3 stops testing the registry.** Without read access to
  the source, a registry that enforces per-repository scopes cannot authorize the
  mount and answers `202` — which says nothing about whether it implements mounting.
  Getting that scope requested is exactly what
  [deduplicated push](push-strategies.md#deduplicated-push) needs its mount sources to
  name their registry for.
- **A registry without a token service** answers the ping with `Basic` rather than
  `Bearer`, and `crane auth token` says so instead of printing a header (Amazon ECR is
  one). Pass the credentials straight to `curl` there: `crane auth get $REG` prints the
  username and secret for `curl -u`.
