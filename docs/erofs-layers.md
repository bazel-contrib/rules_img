# EROFS layers

A layer can be an [EROFS](https://erofs.docs.kernel.org/) filesystem image
instead of a tar changeset. The blob a registry stores is then a filesystem a
runtime can mount directly, rather than an archive it has to unpack.

```python
load("@rules_img//img:layer.bzl", "image_layer")

image_layer(
    name = "app_layer",
    srcs = {"/usr/bin/server": "//cmd/server"},
    compress = "none",
    format = "erofs",
)
```

Or globally:

```
common --@rules_img//img/settings:layer_format=erofs
common --@rules_img//img/settings:compress=none
```

Everything a tar layer gives you still works: up-front digests, the
[compact stream representation](compact-stream.md), the `mtree` output group,
deduplication, content manifests, annotations, and every push and deploy
strategy. The layer blob is opaque to all of them.

**Read [Layers are filesystems, not changesets](#layers-are-filesystems-not-changesets)
before adopting this.** It is the one place where an EROFS layer cannot reproduce
tar-layer behaviour, and it is a property of the format rather than of this
implementation.

## Why

- **Mount instead of unpack.** A runtime with the EROFS snapshotter mounts the
  blob and composes the image with overlayfs. There is no per-file extraction
  step and no second copy of the layer on disk.
- **Per-file content addressing for free.** Because rules_img plans the image
  layout itself and already knows every input file's digest, a file's payload is
  a contiguous, block-aligned, verbatim range of the blob whose checksum *is*
  that file's digest.

## Compression

| `compress` | Media type                            |
|------------|---------------------------------------|
| `none`     | `application/vnd.erofs.layer.v1`      |
| `zstd`     | `application/vnd.erofs.layer.v1+zstd` |

`gzip` is rejected: there is no `application/vnd.erofs.layer.v1+gzip`. `none` is
the default for an EROFS layer and the useful one — an uncompressed blob is
mountable as it is, where a compressed one has to be decompressed first.

These are the only EROFS layer media types rules_img produces or recognizes. A
blob carrying some other spelling is not treated as an EROFS layer, so a pulled
image that uses one is handled as an opaque layer; `media_type = ...` can still
set whatever a consumer needs on a layer rules_img builds.

## Layers are filesystems, not changesets

rules_img omits intermediate directory entries from tar layers by default
(`create_parent_directories = "disabled"`), so writing `/foo/bar/baz.txt`
produces no entry for `/foo` and none for `/foo/bar`. In a tar layer that is
meaningful: a layer is a *changeset*, and "no entry for `/foo`" means "do not
touch `/foo`".

An EROFS layer is a filesystem image. There is no way to reference
`/foo/bar/baz.txt` without directory inodes for `/foo` and `/foo/bar`, so the
builder has to invent them.

That matters because of how overlayfs composes directories. The kernel
documentation is explicit: *"Only the lists of names from directories are merged.
Other content such as metadata and extended attributes are reported for the
upper directory only"*. So an invented `0755 root:root` `/foo` in an upper EROFS
layer **shadows** a base image's `/foo` that was, say, `0700 postgres:postgres`.
The directory's *contents* still merge correctly — a lower-layer
`/foo/other.txt` stays visible — but its mode, owner, timestamps and extended
attributes come from the invented inode.

A directory missing from *every* layer is not a regression: EROFS invents
`0755 root:root`, which is what a container runtime's tar applier produces for a
missing parent anyway.

### What to do about it

**Describe the directories.** An entry the layer describes explicitly is never
invented, whatever the order: describing `/foo` after something beneath it still
counts. `base_image_layer` (see [base images](base-images.md)) is the direct way
to do that, and its metadata streams are read as ordinary layer entries, so the
directory gets exactly the mode and ownership they record.

**Or keep the whole rootfs in one layer.** rules_img's bespoke base-image
content rules (`linux_skeleton`, `etc`, `trust_store`, `system_libraries`) plus
one EROFS layer produce a complete rootfs with no lower layers at all, which
sidesteps the problem entirely. This is the recommended shape for an EROFS
image.

**Or choose what an invented directory looks like:**

```
common --@rules_img//img/settings:erofs_synthetic_directories=inherit
```

| Value     | Behaviour                                                                                                                                 |
|-----------|-------------------------------------------------------------------------------------------------------------------------------------------|
| `inherit` | (default) mode `0755` plus the nearest described ancestor's group- and other-writable bits, with that ancestor's ownership and timestamp — what `mkfs.erofs` does, so an image converted by either producer agrees. |
| `default` | `0755` owned by root, ignoring the surroundings.                                                                                            |
| `strict`  | Fail the layer instead of guessing, naming the directories that would have been invented.                                                   |

`create_parent_directories` is inert for an EROFS layer: it exists to *avoid*
creating parents, and an EROFS layer always has them.

## Whiteouts

A tar layer deletes a path with a `.wh.<name>` marker file and hides a
directory's lower-layer contents with `.wh..wh..opq`. An EROFS layer uses the
overlayfs-native forms instead, and no `.wh.` name ever reaches the image:

| Tar                   | EROFS                                              |
|-----------------------|----------------------------------------------------|
| `.wh.<name>`          | a character device with device number 0 at `<name>` |
| `.wh..wh..opq`        | `trusted.overlay.opaque=y` on the directory         |

`layer_from_tar(format = "erofs")` and `--import-tar` translate markers in
their input, so a converted tar layer deletes the same paths it did before. `img
mtree`'s applied-changeset layout understands both forms, so a layer stack
rendered from the layer blobs applies an EROFS layer's deletions correctly.

One limitation: an image's merged `mtree` output group is assembled from the
*per-layer* mtree files, and mtree has no keyword for a device number. An EROFS
whiteout therefore survives into a merged image mtree as an ordinary
`type=char` entry instead of removing the lower-layer path. A tar layer is not
affected, because its whiteout is a path (`.wh.<name>`) and paths do survive.

## Deduplication

Unchanged from tar layers, and interchangeable with them: the content and node
hashes a content manifest records are defined over the same normalized tar
header, so `--deduplicate` and `--content-manifest` mean the same thing to either
sink.

A repeated file becomes a real EROFS hardlink — one inode, one payload, several
directory entries — which is what a tar layer expresses with a hardlink entry.
The link counts, and therefore the mtree, come out identical.

## Compact streams

`experimental_compact_layers` works unchanged, and this is the reason the
implementation looks the way it does. The image is laid out metadata-first, so
the blob is:

```
offset 0                                                            blob end
├─ inline ──────────┼─ CAS ref ─┬ inline ─┬─ CAS ref ─┬ inline ─┬ … ──┤
   superblock +       file A      padding   file B      padding
   metadata           payload               payload     + dirents
```

Every file payload is a contiguous, block-aligned, verbatim copy of the input
file, so it becomes exactly one content-addressed reference; everything else —
superblock, inodes, extended attributes, tail-packed small files, directory
blocks, symlink targets and all padding — is carried inline. That is
structurally the same shape as a compact stream over a tar, so reconstruction,
`deployvfs`, the BES syncer and every push strategy need no EROFS-specific code
at all.

Two consequences worth knowing:

- **Reconstruction of a `+zstd` layer is pinned to the zstd implementation.** The
  compact stream rebuilds the blob by re-compressing, so a layer digest depends
  on `klauspost/compress` producing the same bytes. This is already true of
  tar+zstd layers, but zstd is the *only* compressed option here, so it becomes
  load-bearing. `compress = "none"` avoids it entirely.
- **Small files ride along inline.** EROFS tail-packs a payload that fits in the
  remainder of its inode's block, so it lands in the byte stream rather than the
  CAS. `experimental_compact_layers_inline_threshold` caps that, so a file at
  least that large always gets an extent (and therefore a reference) of its own.

Each region the compact stream replaces with a reference is re-hashed as it is
written and checked against the recorded digest, which turns a wrong layout plan
into a build failure instead of a compact stream that only fails when someone
reconstructs the layer. `--erofs-no-verify-cas-refs` disables the check.

## mtree

The `mtree` output group works for EROFS layers, including in compact-stream
mode: because all of the image's metadata is inline, the tree can be walked from
the compact stream alone, with the file payloads absent, and each file's digest
comes from the reference table.

An EROFS layer's mtree differs from the same layer rendered as a tar in three
ways, all inherent to the format:

- every intermediate directory is present (see above);
- directories carry real link counts (2 plus one per subdirectory), which a tar
  layer has no way to express;
- `uname` and `gname` are absent. An EROFS inode stores numeric ids only, as the
  kernel does, so the owner *names* a tar header can carry are dropped. A
  structure test that asserts on them has to assert on `uid`/`gid` instead.

Everything else agrees, which is what `TestMtreeEquivalence` in
`img_tool/pkg/mtree` asserts.

## Manifests and images

`img manifest` derives what the format requires rather than asking:

- `erofs` is added to `os.features`, in the config and in the index's platform
  descriptor, so a runtime that cannot mount these layers does not try.

**A manifest may not mix tar and EROFS layers.** A runtime applies a tar layer by
extracting it and an EROFS layer by mounting it, so a stack with both cannot be
composed; `img manifest` fails with the offending layers named. The usual way to
hit this is pulling a tar base image and building EROFS layers on top. Convert
the base with `layer_from_tar(format = "erofs")`, or build the whole image
as tar layers.

Pulled EROFS base images need nothing special: `image_import` and `pull` treat
layer blobs as opaque and carry the media type through.

## Loading

Only containerd with the EROFS snapshotter can apply these layers. `image_load`
fails at analysis time for `docker`, `podman`, `tar` and `containerization`, and
passes through for `containerd` and `generic`.

## Not supported

| Feature                                   | Why                                                                          |
|-------------------------------------------|------------------------------------------------------------------------------|
| gzip compression                          | no `application/vnd.erofs.layer.v1+gzip` media type                                   |
| eStargz                                   | tar-specific                                                                 |
| SOCI / ztoc                               | gzip-tar-specific                                                            |
| `image_load` to docker/podman             | those daemons cannot apply a filesystem image                                 |
| omitting intermediate directories         | a filesystem needs a complete directory tree                                  |
| mixing tar and EROFS layers in a manifest | the two are applied by different mechanisms                                   |
| per-file EROFS compression (`-z`)         | it would destroy the "payload = verbatim file bytes" property the compact stream and per-file content addressing rely on; outer `+zstd` covers wire size |
| cross-layer hardlinks                     | a hardlink is a second dirent for an inode in the same image; a link into a lower layer fails loudly |

dm-verity is part of the layer format but not yet emitted.

## Tuning

```
# Filesystem block size. 4096 matches the common page size, so the image mounts
# without kernel support for block sizes below the page size.
common --@rules_img//img/settings:erofs_block_size=4096
```

## Reproducibility

EROFS images built by rules_img are byte-identical across machines and rebuilds:
the UUID is zero, the build timestamp is zero, directory entries and extended
attributes are sorted, and the layout is a pure function of the layer's inputs
plus the synthesized-directory policy.

One thing to know: an inode can use the compact 32-byte form only when its mtime
equals the image's build timestamp, which is zero. rules_img's default of an
unset mtime therefore produces the smallest images; setting a non-zero `mtime`
through `default_metadata` makes every inode 64 bytes.
