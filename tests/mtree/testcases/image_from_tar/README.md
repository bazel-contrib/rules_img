# image_from_tar mtree testcase

`layer.tar` is a small, deterministic, hand-built tar used as a raw layer for an
`image_manifest` (`layers = ["layer.tar"]`). Because it is added via `DefaultInfo`
rather than a rules_img layer rule, it carries no precomputed `mtree`, so the
`image_manifest` rule must render one **on the fly** from the tar blob — the same
code path taken for pulled / imported base-image layers that ship a blob but no
mtree (a layer with no blob at all, i.e. a shallow image, is skipped).

Contents (all entries: uid/gid 0, uname/gname `root`, mtime `1609459200` =
2021-01-01T00:00:00Z):

- `etc/` — an explicit directory entry (kept with its metadata)
- `etc/hosts` — regular file, mode 0644, `"127.0.0.1 localhost\n"`
- `opt/tool` — regular file, mode 0755, `"#!/bin/sh\n"`; note there is **no**
  `opt/` entry, so the applied-changeset merge synthesizes `./opt` as `type=dir`
  only

Regenerate with:

```python
import tarfile, io
MT = 1609459200
def ti(name, typ, mode, size=0):
    t = tarfile.TarInfo(name); t.type = typ; t.mode = mode; t.mtime = MT
    t.uid = 0; t.gid = 0; t.uname = "root"; t.gname = "root"; t.size = size
    return t
with tarfile.open("layer.tar", "w", format=tarfile.PAX_FORMAT) as tw:
    tw.addfile(ti("etc/", tarfile.DIRTYPE, 0o755))
    b = b"127.0.0.1 localhost\n"
    tw.addfile(ti("etc/hosts", tarfile.REGTYPE, 0o644, len(b)), io.BytesIO(b))
    b = b"#!/bin/sh\n"
    tw.addfile(ti("opt/tool", tarfile.REGTYPE, 0o755, len(b)), io.BytesIO(b))
```

To regenerate `expected.mtree`, build the test's extract target and copy its
output:

```
bazel build //tests/mtree/testcases/image_from_tar:test_disabled_mtree
cp bazel-bin/tests/mtree/testcases/image_from_tar/image.mtree expected.mtree
```
