# Extent streams (experimental)

> **Experimental.** The extent stream format, the `img extent-stream` command and
> the `extent_stream` attribute of `disk_image` can change incompatibly in any
> release. Every stream is marked as experimental in its header, and the tools
> print a warning.

A [compact stream](compact-stream.md) is one byte sequence with a table of
content-addressed references cut out of it. That is a good fit for a tar layer or
for a single filesystem image. It does not compose: a GPT disk contains a FAT32
partition, which contains a cpio archive, which contains large files from the CAS.

An **extent stream** describes a byte sequence (the *content*) as an ordered
list of extents. It is a second format next to the compact stream, which is
unchanged. Each extent is one of:

| Kind             | Meaning |
|------------------|---------|
| `data`           | Bytes embedded in the stream. |
| `hole`           | Zeros that are never stored. A file that is reconstructed stays sparse. |
| `cas`            | A range of a blob in a content-addressed store, addressed by its SHA-256. |
| `compact-stream` | A range of the *reconstructed content* of a compact stream, which is addressed by the SHA-256 of its `.cstream` file. |
| `extent-stream`  | A range of the content of another extent stream, addressed by the SHA-256 of its `.xstream` file. |

The extents tile the content: the first starts at offset 0, each one starts where
the previous one ends, and the lengths add up to the size in the header.

## Nesting

Streams are referenced by the digest of their file, not copied into the stream
that uses them. The parent stays small, a child that several parents use is stored
once, and nesting needs no special case: a disk image references the compact
stream of its FAT32 partition by digest, and that stream references the files in
it. Because a digest cannot refer to a stream that contains it, references cannot
form a cycle; readers also limit the nesting to 16 levels.

Every reference carries a range, which replaces the "patches" a flat format would
need. A FAT32 file system whose hidden-sector field is set to the first sector of
its partition becomes three extents:

```
compact-stream  sha256:<fs>  content [0, 28)
data            <4 bytes>
compact-stream  sha256:<fs>  content [32, size)
```

## Using it

```
img disk-image --extent-stream --output disk.xstream --output-cas-dir disk.cas ...
img extent-stream list disk.xstream
img extent-stream reconstruct --extent-stream disk.xstream \
    --cas-dir disk.cas --cas-dir layer.inputfilecas --output disk.img
```

`--cas-dir` can be repeated; a blob or stream is taken from the first directory
that has it. `img dd` (see [disk images](disk-images.md#writing-a-disk-image-or-a-layer-image_dd))
also reads from the Bazel disk cache and the remote cache, so a stream can be
reconstructed without the files it references being downloaded first, and it
writes a range with `skip` and `count`. The directories use the layout of `img cas-dir`
(`sha256/<first two hex digits>/<hex>`; the flat `sha256/<hex>` layout is read as well).

The `disk_image` rule produces an extent stream with `extent_stream = "enabled"`;
see [disk images](disk-images.md).

## Integrity

- A referenced **stream** is hashed when it is fetched and must match the digest and
  size in the extent.
- A referenced **blob** is only checked for its length, like the blobs of a compact
  stream. A header can record the SHA-256 of the whole content (flag bit 0), which
  `Reconstruct` verifies. `disk_image` does not record it, because computing it
  needs the content of every referenced file.

## Specification (version 1)

All integers are big-endian. A stream is `[header][extent table][data section]`,
which follow each other without padding.

### Header (128 bytes)

```
Offset  Size  Type       Field
0       6     bytes      Magic: "CASEXT"
6       1     uint8      0
7       1     uint8      Version (1)
8       2     uint16     HashAlgorithm (1 = SHA-256)
10      2     uint16     HashSize (32)
12      1     uint8      DataCompression (0 = none, 1 = zstd)
13      1     uint8      Flags
14      1     uint8      ContentFormat (0 = unknown, 1 = disk)
15      1     uint8      Reserved (0)
16      8     uint64     Size: size of the content
24      8     uint64     ExtentCount
32      8     uint64     TableOffset (128)
40      8     uint64     TableSize (ExtentCount * 64)
48      8     uint64     DataOffset (TableOffset + TableSize)
56      8     uint64     DataSize: size of the data section on disk
64      8     uint64     DataRawSize: size of the data section, uncompressed
72      56    bytes      ContentDigest: SHA-256 of the content in the first 32 bytes
```

Flags: bit 0 = `ContentDigest` is valid, bit 1 = experimental (set by every version-1
writer). A reader rejects an unknown version, hash algorithm or compression.

### Extent table (64 bytes per extent)

```
Offset  Size  Type       Field
0       1     uint8      Kind: 1 data, 2 hole, 3 cas, 4 compact-stream, 5 extent-stream
1       7     bytes      0
8       8     uint64     Length (non-zero)
16      8     uint64     SrcOffset
24      8     uint64     BlobSize
32      32    bytes      Digest
```

| Kind | `SrcOffset` | `BlobSize`, `Digest` |
|------|-------------|----------------------|
| data | offset in the uncompressed data section; data extents are contiguous in it | 0 |
| hole | 0 | 0 |
| cas | offset in the blob (`SrcOffset + Length <= BlobSize`) | size and digest of the blob |
| compact-stream, extent-stream | offset in the reconstructed content of the stream | size and digest of the stream file |

### Data section

The concatenated bytes of all `data` extents, in order, compressed as one zstd
stream when `DataCompression` is 1. Reconstruction reads it sequentially.

### Reconstruction

Walk the extents in order and write each one: `data` from the data section,
`hole` as zeros (or by seeking), `cas` by copying the range of the blob, and the
two stream kinds by reconstructing the referenced stream and copying the range. A
reader validates that the extents tile `Size`, that every `cas` range is inside its
blob, and that the data extents add up to `DataRawSize`.

## Go API

The package is `img_tool/pkg/extentstream`: `Writer` (`AddData`, `AddHole`,
`AddRef`, `AddRefPatched`), `Inspect` and `Reconstruct`. It reads blobs through the
`BlobStore` interface of `compactstream`, so the same directories and stores work
for both formats.

## Not (yet) supported

- Layer rules (ext4, FAT32, cpio, EROFS) do not write extent streams. The format is
  designed so that they can: a FAT32 image would be data for the metadata and
  `cas` extents for its files, and a cpio archive in a file of it a reference to
  an extent stream.
- Extent streams are not valid OCI layers; `img deploy` does not read them.
- A flattened view of all the leaf references (for listing the files in a disk).
