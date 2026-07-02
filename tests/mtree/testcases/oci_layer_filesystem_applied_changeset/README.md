# `oci_layer_filesystem_applied_changeset` mtree testcase

`layer.tar` is a small, hand-crafted tar that exercises everything a layer can
carry, so the `oci_layer_filesystem_applied_changeset` mtree layout is tested
end to end. It is generated deterministically (fixed mtime `1600000000`) and
checked in; `expected.mtree` is the golden produced by `img mtree` with the layer
layout pinned to the applied-changeset setting and every field enabled.

Tar entries, in order (this is the raw changeset, before it is applied):

| entry               | type     | notes                                                        |
| ------------------- | -------- | ------------------------------------------------------------ |
| `etc/`              | dir      | explicit directory with full ownership metadata              |
| `etc/config`        | file     | all metadata: mode 0640, uid/gid 1000, uname alice, gname devs, content (sha256) |
| `etc/config.sym`    | symlink  | `-> config` (the `link` keyword is symlink-only)             |
| `etc/config.hard`   | hardlink | `-> etc/config` (rendered as a copy of `etc/config`; nlink=2) |
| `etc/secret`        | file     | extended attribute `SCHILY.xattr.user.owner=alice`           |
| `usr/local/bin/tool`| file     | parents `usr`, `usr/local`, `usr/local/bin` are missing      |
| `var/old.log`       | file     | removed below by a whiteout                                  |
| `var/.wh.old.log`   | whiteout | deletes `var/old.log` (the marker itself is consumed)        |
| `srv/`              | dir      | explicit directory                                           |
| `srv/keep`          | file     | kept                                                         |
| `srv/.wh..wh..opq`  | opaque   | opaque whiteout: consumed (nothing below an empty base to hide) |
| `run/fifo`          | fifo     | parent `run` is missing; renders as `type=fifo`              |

Applying this changeset to an empty filesystem yields (what `expected.mtree`
describes, in stable path-sorted order):

- `etc`, `etc/config`, `etc/config.hard` (copy of `etc/config`), `etc/config.sym`, `etc/secret`
- `run` (synthesized), `run/fifo`
- `srv`, `srv/keep`
- `usr`, `usr/local`, `usr/local/bin` (all synthesized), `usr/local/bin/tool`
- `var` (synthesized; `var/old.log` was whited out)

Synthesized parent directories carry only `type=dir`. Whiteout markers do not
appear in the output.

To regenerate `layer.tar`, craft the entries above with `archive/tar`
(`Format: tar.FormatPAX`, mtime `time.Unix(1600000000, 0).UTC()`).
