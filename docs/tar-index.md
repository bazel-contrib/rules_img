# CAS Stream Index Format (v1)

A CAS stream index is a compact representation of a byte stream (optionally
compressed) where contiguous ranges of the uncompressed data are replaced by
content-addressed storage (CAS) references. The original stream can be
reconstructed bit-for-bit from the index plus the referenced blobs retrieved
from a content-addressed store.

## Overview

An index file consists of three parts:

1. A **80-byte uncompressed file header** containing format metadata, hash
   algorithm, compression settings, and offset/size pointers to the
   data sections.
2. A **CAS reference table** (uncompressed) listing the offsets, digests,
   and sizes of data replaced by CAS references.
3. An optional **local path table** (uncompressed) containing null-terminated
   local file paths corresponding to each CAS reference entry.
4. A **byte stream** (optionally zstd-compressed) containing the remaining
   bytes of the original stream with the CAS-referenced ranges removed.

```
+-------------------------------------------+
|  File Header (80 bytes, uncompressed)     |
+-------------------------------------------+
|  CAS Reference Table (uncompressed)       |
+-------------------------------------------+
|  Local Path Table (optional, uncompressed)|
+-------------------------------------------+
|  Byte Stream (optionally zstd-compressed) |
+-------------------------------------------+
```

## File Header

The file header is always uncompressed so that readers can identify the format
and determine how to locate and decompress the data sections.

```
Offset  Size  Type       Field                 Description
------  ----  ---------  --------------------  -------------------------------------------
0       6     ASCII      Magic                 "CASSTR"
6       1     uint8      NUL                   Always 0x00
7       1     uint8      Version               Format version (0x01)
8       2     uint16 BE  HashAlgorithm         Digest algorithm for content hashes
10      2     uint16 BE  HashSize              Digest size in bytes
12      1     uint8      StreamCompression     On-disk compression of the byte stream section
13      1     uint8      OriginalCompression   Compression of the original file
14      1     uint8      SeekableCompression   Whether seekable compression was used (0/1)
15      1     int8       CompressionLevel      Original compression level (-1 = default)
16      1     uint8      CompressorJobs        Number of parallel compression workers (0 = default)
17      3     -          Reserved1             Must be zero
20      4     uint32 BE  EndPadding            Trailing zero bytes in the original stream
24      8     uint64 BE  RefTableOffset        Byte offset from file start to CAS ref table
32      8     uint64 BE  RefTableSize          Size in bytes of the CAS reference table
40      8     uint64 BE  StreamOffset          Byte offset from file start to byte stream
48      8     uint64 BE  StreamSize            Size in bytes of the byte stream (on disk)
56      8     uint64 BE  LocalPathTableOffset  Byte offset to local path table (0 = absent)
64      8     uint64 BE  LocalPathTableSize    Size in bytes of the local path table
72      8     -          Reserved2             Must be zero
```

Total: 80 bytes.

### Hash algorithm values

| Value | Algorithm |
|-------|-----------|
| 1     | SHA-256   |

### Stream compression values

| Value | Compression |
|-------|-------------|
| 0     | None        |
| 1     | zstd        |

### Original compression values

| Value | Compression |
|-------|-------------|
| 0     | None        |
| 1     | gzip        |
| 2     | zstd        |

### Compression metadata

The file header records two distinct compression concepts:

- **StreamCompression**: How the byte stream section is compressed on disk
  within this index file (for storage efficiency). Readers decompress this
  section before using its contents.

- **OriginalCompression**: What compression the original file uses. After
  reconstruction (merging stream bytes with CAS blobs), the result is the
  uncompressed original. To obtain the original compressed form, re-compress
  using `OriginalCompression`, `SeekableCompression`, `CompressionLevel`,
  and `CompressorJobs`.

- **SeekableCompression**: `1` if seekable compression was applied (e.g.
  estargz for container image layers), `0` otherwise.

- **CompressionLevel**: The compression level as a signed byte. `-1` means
  the library default. For gzip: 0-9. For zstd: values fit within int8 range.

- **CompressorJobs**: Number of parallel compression workers. `0` means
  default. `1` means single-threaded. Values > 1 indicate parallel compression.

- **EndPadding**: Number of trailing zero bytes appended after the original
  stream content. For tar files, standard archives end with two 512-byte zero
  blocks (1024 bytes), but the rules_img tooling does not add end-of-archive
  padding, so this value is typically `0`.

## CAS Reference Table

The reference table is a flat array of fixed-size entries, sorted by `Offset`
ascending. Each entry describes one contiguous range in the uncompressed
original stream that has been replaced by a CAS reference:

```
Offset  Size      Type       Field    Description
------  --------  ---------  -------  -------------------------------------------
0       8         uint64 BE  Offset   Byte offset in the reconstructed uncompressed stream
8       HashSize  bytes      Digest   Content digest of the replaced data
8+HS    8         uint64 BE  Size     Number of bytes replaced by this CAS reference
```

Entry size = `16 + HashSize` bytes (48 bytes for SHA-256).

The number of entries is `RefTableSize / (16 + HashSize)`.

Key properties:
- Entries are sorted by `Offset` ascending.
- Entries must not overlap: for any two adjacent entries `i` and `i+1`,
  `entries[i].Offset + entries[i].Size <= entries[i+1].Offset`.
- Fixed-size entries enable O(1) random access by index.

## Local Path Table

The local path table is an optional section that records the local filesystem
path for each CAS reference entry. It is present when `LocalPathTableOffset`
is non-zero.

The table is a simple concatenation of null-terminated strings, one per CAS
reference entry (in the same order as the reference table). Each string is
the absolute or relative path to the source file on the machine that produced
the index.

During reconstruction, the local path table serves as a **lookaside cache**:
before fetching a blob from the remote CAS, the reader checks whether the
file at the recorded local path still exists and has the expected digest. If
so, the local file is used directly, avoiding the CAS lookup.

If a local path entry is empty (a single NUL byte for that entry), no local
lookaside is attempted for that reference.

## Byte Stream

The byte stream contains the bytes of the original uncompressed file with
the CAS-referenced ranges removed. Specifically, if the original uncompressed
file is `F` of some length, and there are CAS references at offsets
`[o_0, o_1, ..., o_{n-1}]` with sizes `[s_0, s_1, ..., s_{n-1}]`, then
the stream is the concatenation of:

```
F[0 : o_0] || F[o_0+s_0 : o_1] || F[o_1+s_1 : o_2] || ... || F[o_{n-1}+s_{n-1} : len(F)]
```

If `StreamCompression` is non-zero, the stream section is compressed with the
specified algorithm. `StreamSize` is the on-disk (compressed) size.

## Reconstruction

To reconstruct the original uncompressed file from an index:

1. Read the 80-byte file header. Validate the magic and version. Extract
   the hash size, stream compression, and section offsets/sizes.
2. Read the CAS reference table (`RefTableSize` bytes at `RefTableOffset`).
   Parse into a sorted list of `(Offset, Digest, Size)` entries.
3. If `LocalPathTableOffset` is non-zero, read the local path table and
   associate each path with the corresponding reference entry.
4. Open the byte stream at `StreamOffset`. If `StreamCompression` is non-zero,
   create a decompressor.
5. Initialize `output_pos = 0`, `ref_idx = 0`.
6. Loop while `ref_idx < ref_count`:
   a. `gap = refs[ref_idx].Offset - output_pos`
   b. Copy `gap` bytes from the stream to the output.
   c. Attempt to read the blob from the local path (if available):
      - If the local path is non-empty AND the file exists AND has the
        expected size AND its content digest matches `refs[ref_idx].Digest`,
        use the local file content.
      - Otherwise, fetch the blob from the CAS using the digest.
   d. Write the blob bytes to the output.
   e. `output_pos = refs[ref_idx].Offset + refs[ref_idx].Size`
   f. `ref_idx++`
7. Copy all remaining bytes from the stream to the output.

The result is the original uncompressed file. To obtain the original
compressed form:

7. If `OriginalCompression != 0`, re-compress the output using the settings
   from the header (`OriginalCompression`, `SeekableCompression`,
   `CompressionLevel`, `CompressorJobs`).
8. Append `EndPadding` zero bytes.

## Usage

The layer command produces an index file when the `--cas-index` flag is set:

```bash
img layer \
  --add /app/bin/server=./server \
  --cas-index layer.casstr \
  layer.tgz
```

To inline small files (e.g. below 4096 bytes) directly in the stream
instead of emitting CAS references:

```bash
img layer \
  --add /app/bin/server=./server \
  --cas-index layer.casstr \
  --cas-index-inline-threshold 4096 \
  layer.tgz
```

When `--cas-index-inline-threshold` is set, files smaller than the threshold
have their content stored directly in the byte stream (no CAS reference is
emitted). This eliminates CAS lookups during reconstruction for small files.

To record local file paths for faster reconstruction without CAS:

```bash
img layer \
  --add /app/bin/server=./server \
  --cas-index layer.casstr \
  --cas-index-local-paths \
  layer.tgz
```

When `--cas-index-local-paths` is set, the index records the source file paths
used during layer creation. During reconstruction, these paths serve as a
lookaside cache: if the file still exists locally with the correct content, it
is used directly instead of fetching from the CAS.

The byte stream is zstd-compressed by default. The original compression
metadata is automatically derived from the layer's compression settings.
