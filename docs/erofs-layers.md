# EROFS layers

A layer can be an [EROFS](https://erofs.docs.kernel.org/) filesystem image
instead of a tar changeset. This allows container runtimes (such as containerd) to
mount the layer directly (and skip the layer unpacking step).

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

## Layer compression

| `compress` | Media type                            |
|------------|---------------------------------------|
| `none`     | `application/vnd.erofs.layer.v1`      |
| `zstd`     | `application/vnd.erofs.layer.v1+zstd` |

`gzip` compression is rejected since containerd doesn't support `application/vnd.erofs.layer.v1+gzip`.

## File compression

An EROFS layer can store the data of its files compressed, which the kernel
undoes as they are read. That makes a layer smaller on disk and in the page cache
without unpacking it first, which the outer `+zstd` compression cannot do: that
one has to be undone before the filesystem can be mounted.

```python
load("@rules_img//img:layer.bzl", "erofs_config", "image_layer")

erofs_config(
    name = "lz4",
    compression = "lz4",
)

image_layer(
    name = "app_layer",
    srcs = {"/usr/bin/server": "//cmd/server"},
    compress = "none",
    format = "erofs",
    format_config = ":lz4",
)
```

Compression is per file, in one-block physical clusters, with LZ4 (`"lz4"`),
DEFLATE (`"deflate"`) or Zstandard (`"zstd"`, which compresses best and is the
slowest to build). A file that is
no larger than a block, or in which nothing compresses, is stored as it is, so
incompressible data costs no index space. The mounted layer needs a kernel with
compressed-EROFS support: Linux 5.3 or newer for LZ4, Linux 6.6 or newer for
DEFLATE and Linux 6.10 or newer for Zstandard (with `CONFIG_EROFS_FS_ZIP_DEFLATE`
or `CONFIG_EROFS_FS_ZIP_ZSTD`). Both are read back by erofs-utils
(`fsck.erofs`, `dump.erofs`). `compress = "none"` is the right outer compression
for such a layer: compressing the blob again gains little.

By default every compressed cluster is one block. `cluster_size` lets a cluster
take up to that many bytes (16384 or 65536 are reasonable), which compresses
noticeably better because more input is compressed together, at the cost of
decompressing more to read a small part of a file. Clusters of more than one
block need Linux 5.13 or newer.

The mtree of a compressed layer is the one of the uncompressed layer, because file
digests are digests of content.

File compression and [compact streams](#compact-streams) are exclusive. A
compressed file is not a verbatim copy of its input, so a compact stream cannot
reference it. With `experimental_compact_layers` enabled, a layer with compressed
files is built in full as a blob instead.

## Layers are filesystems, not changesets

rules_img omits intermediate directory entries from tar layers by default
(`create_parent_directories = "disabled"`), so writing `/foo/bar/baz.txt`
produces no tar entry for `/foo` and none for `/foo/bar`. In a tar layer that is
meaningful: a layer is a *changeset*, and "no entry for `/foo`" means "do not
touch `/foo`".

An EROFS layer is a filesystem image. There is no way to reference
`/foo/bar/baz.txt` without directory inodes for `/foo` and `/foo/bar`, so the
builder has to create them.

That matters because of how overlayfs composes directories. The kernel
documentation is explicit: *"Only the lists of names from directories are merged.
Other content such as metadata and extended attributes are reported for the
upper directory only"*. So an invented `0755 root:root` `/foo` in an upper EROFS
layer **shadows** a base image's `/foo` that was, say, `0700 postgres:postgres`.
The directory's *contents* still merge correctly — a lower-layer
`/foo/other.txt` stays visible — but its mode, owner, timestamps and extended
attributes come from the invented inode.

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
mtree` translates the EROFS forms back to tar-style whiteout markers, including
opacity on the filesystem root. Per-layer mtree files preserve these markers so
the applied-changeset layout can apply deletions when merging layers.

## Deduplication

Unchanged from tar layers, and interchangeable with them: the content and node
hashes a content manifest records are defined over the same normalized tar
header, so `--deduplicate` and `--content-manifest` mean the same thing to either
sink.
A repeated file becomes a real EROFS hardlink.

## Compact streams

`experimental_compact_layers` works unchanged. The image is laid out metadata-first, so
the blob is:

```
offset 0                                                            blob end
├─ inline ──────────┼─ CAS ref ─┬ inline ─┬─ CAS ref ─┬ inline ─┬ … ──┤
   superblock +       file A      padding   file B      padding
   metadata           payload               payload     + dirents
```

Every file payload is a contiguous, block-aligned, verbatim copy of the input
file, so it can be replaced by exactly one content-addressed reference. A layer
with [file compression](#file-compression) has no such payloads and is not
written as a compact stream.

## mtree

The `mtree` output group works for EROFS layers, including in compact-stream
mode.
An EROFS layer's mtree differs from the same layer rendered as a tar in three
ways, all inherent to the format:

- every intermediate directory is present (see above);
- directories carry real link counts (2 plus one per subdirectory), which a tar
  layer has no way to express
- `uname` and `gname` are absent. An EROFS inode stores numeric ids only, as the
  kernel does, so the owner *names* a tar header can represent are dropped. A
  structure test that asserts on them has to assert on `uid`/`gid` instead.

## Manifests and images

When building a conatiner image manifest that includes EROFS layers, `erofs`
is added to `os.features`, so a container runtime can select a matching image
from an image index if it supports mounting EROFS layers.

## Not supported

| Feature                                   | Why                                                                          |
|-------------------------------------------|------------------------------------------------------------------------------|
| gzip compression                          | no `application/vnd.erofs.layer.v1+gzip` media type                                   |
| eStargz                                   | tar-specific                                                                 |
| SOCI / ztoc                               | gzip-tar-specific                                                            |
| omitting intermediate directories         | a filesystem needs a complete directory tree                                  |
| LZMA file compression                     | only LZ4, DEFLATE and Zstandard are implemented; see [File compression](#file-compression) |
| compacted indexes, fragments, deduplication, tail packing of compressed data | not used: compressed files use full indexes. The result is valid EROFS, but larger than `mkfs.erofs -zlz4hc` can make it |
| cross-layer hardlinks                     | a hardlink is a second dirent for an inode in the same image |

dm-verity is part of the layer format but not yet supported by rules_img.

## Tuning

The options of the image come from an `erofs_config` target, passed to the
layer's `format_config` attribute:

```python
load("@rules_img//img:layer.bzl", "erofs_config", "image_layer")

erofs_config(
    name = "layer_config",
    # Filesystem block size. 4096 matches the common page size, so the image
    # mounts without kernel support for block sizes below the page size.
    block_size = 4096,
)

image_layer(
    name = "layer",
    srcs = {"/etc/hostname": ":hostname"},
    format = "erofs",
    format_config = ":layer_config",
)
```

These options also have a global default, for setting them across a
build:

```
common --@rules_img//img/settings:erofs_inline_threshold=0
common --@rules_img//img/settings:erofs_synthetic_directories=inherit
common --@rules_img//img/settings:erofs_compression=zstd
common --@rules_img//img/settings:erofs_cluster_size=16384
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
