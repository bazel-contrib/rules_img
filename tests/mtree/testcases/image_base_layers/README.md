# image_base_layers mtree testcase

This exercises `mtree` for a **base image whose layers carry a blob but no
precomputed mtree** — the situation for a *pulled* / *imported* base image (whose
layers ship a compressed tar blob but never a rules_img `mtree`). Shallow images,
whose layers have no blob at all, cannot be handled and are skipped.

`base_layer.tar` is a small, deterministic raw tar exposed through a `filegroup`
and used as the only layer of `base_image` (an `image_manifest`). From the
consuming image's point of view, `base_image`'s `ImageManifestInfo.layers` then
looks exactly like a pulled base: a layer with a blob and `mtree = None`.

The image under test (`:image`) sets `base = :base_image` and adds a normal
`image_layer` (`:top_layer`, which does carry its own mtree). The `mtree` output
group therefore merges:

- an mtree rendered **on the fly** from the base layer's blob, followed by
- the top layer's **rule-built** mtree,

in layer order (base first). `expected.mtree` shows both `./usr/bin/base-tool`
(from the base) and `./usr/bin/top-tool` (from the top layer) under a shared,
synthesized `./usr/bin` directory.

`base_layer.tar` regeneration:

```python
import tarfile, io
MT = 1609459200
def ti(name, mode, size):
    t = tarfile.TarInfo(name); t.type = tarfile.REGTYPE; t.mode = mode; t.mtime = MT
    t.uid = 0; t.gid = 0; t.uname = "root"; t.gname = "root"; t.size = size
    return t
with tarfile.open("base_layer.tar", "w", format=tarfile.PAX_FORMAT) as tw:
    b = b"base\n"
    tw.addfile(ti("usr/bin/base-tool", 0o755, len(b)), io.BytesIO(b))
```

`expected.mtree` regeneration:

```
bazel build //tests/mtree/testcases/image_base_layers:test_disabled_mtree
cp bazel-bin/tests/mtree/testcases/image_base_layers/image.mtree expected.mtree
```
