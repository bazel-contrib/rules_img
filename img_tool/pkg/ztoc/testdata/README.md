# ztoc test-vector corpus

This directory holds the golden test vectors for `pkg/ztoc`. Each vector is a
gzip-compressed tar layer (`vectors/<name>.tar.gz`) plus a golden ztoc
(`vectors/<name>.ztoc`) that was produced by **soci-snapshotter's own
builder** — the authoritative, cgo/zlib implementation. `manifest.json` lists
every vector as `{name, input, span}`.

The package tests (`corpus_test.go`) rebuild each ztoc in pure Go and require
the serialized FlatBuffer to be **byte-for-byte identical** to soci's golden.
That single check exercises the whole pipeline at once: the DEFLATE/gzip
decompressor, the zran-style checkpoints (`in`/`out`/`bits`/32 KiB window), the
per-span sha256 digests, the tar TOC extraction, and the FlatBuffer marshaling.

The goldens are built with `build_tool_identifier = "soci-oracle"`, and the
tests set the same identifier so the comparison is exact.

## What the corpus covers

- **Compression levels** including level 0 (stored/uncompressed deflate blocks),
  1, 6 (default), and 9 — exercising stored, fixed-Huffman, and dynamic-Huffman
  blocks and their byte-alignment.
- **Multiple spans / checkpoints** via both small span sizes on small inputs and
  a realistic 4 MiB span on an input larger than one span.
- **Multi-member (concatenated) gzip** streams, as produced by pigz/mgzip.
- **gzip header variants**: `FNAME`, `FCOMMENT`, `FEXTRA`, and combinations,
  which change the header length and therefore the first checkpoint's offset.
- **Rich tar metadata**: regular files, directories, symlinks, hardlinks,
  character/block devices, FIFOs, xattrs and other PAX records, long names and
  link targets, setuid/sticky modes, non-zero uid/gid/uname/gname, and
  sub-second modification times.
- **Edge cases**: an empty archive and a single tiny file.

Inputs are fully deterministic (fixed mtimes, seeded PRNG, no host-dependent
fields), so the corpus is reproducible. Because the inputs are committed, the
goldens stay valid regardless of the Go toolchain version.

## Regenerating / extending the corpus

The generator (`generate/main.go`) writes the deterministic inputs and invokes
an *oracle* binary to produce each golden. The oracle is a tiny program that
links soci-snapshotter's real `ztoc` package (cgo + system zlib). Steps:

1. Clone soci-snapshotter and build the oracle. soci's `ztoc/compression`
   package is Linux-oriented; to build it on macOS a few local edits are needed:
   - `#include <endian.h>` → a small shim (macOS is little-endian, so the
     `htole*`/`le*toh` helpers are the identity).
   - the cgo `LDFLAGS` `-l:libz.a` → `-lz` (link the system zlib).
   - `pt_index_from_ucmp_offset(..., C.long(offset))` →
     `C.offset_t(offset)` (a cgo type mismatch that is only hit at compile time;
     the function is unused when building a ztoc).

   The oracle itself is:

   ```go
   // oracle <input.tar.gz> <spanSize> <out.ztoc>
   package main

   import (
       "io"; "os"; "strconv"
       "github.com/awslabs/soci-snapshotter/ztoc"
   )

   func main() {
       span, _ := strconv.ParseInt(os.Args[2], 10, 64)
       z, err := ztoc.NewBuilder("soci-oracle").
           BuildZtoc(os.Args[1], span, ztoc.WithCompression("gzip"))
       if err != nil { panic(err) }
       r, _, err := ztoc.Marshal(z)
       if err != nil { panic(err) }
       out, _ := os.Create(os.Args[3])
       defer out.Close()
       io.Copy(out, r)
   }
   ```

2. Run the generator, pointing it at the output directory and the oracle:

   ```sh
   go run ./generate <path-to>/vectors <path-to>/oracle
   ```

   This rewrites `vectors/*.tar.gz`, `vectors/*.ztoc`, and `manifest.json`.
