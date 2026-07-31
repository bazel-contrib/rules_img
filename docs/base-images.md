# Building bespoke base images

Most images start `FROM` something. When that something has to be *yours* — a
locked-down distroless base, an image with your corporate CA already trusted, a
minimal filesystem for a static binary — you need to describe a base image
rather than extend one.

rules_img has a family of rules for exactly that. They divide into two kinds:

- **Content rules** describe what should be in the image — a directory
  skeleton, users and groups, a CA trust store, shared libraries, the standard
  files under `/etc`. None of them builds a layer.
- **`base_image_layer`** takes any number of those descriptions and merges them
  into a single flat layer.

```starlark
load(
    "@rules_img//img:base_images.bzl",
    "base_image_layer",
    "etc_passwd",
    "linux_skeleton",
    "passwd_entry",
    "trust_store",
)
load("@rules_img//img:image.bzl", "image_manifest")

linux_skeleton(name = "skeleton")

etc_passwd(
    name = "users",
    users = [passwd_entry(username = "app", uid = 1000, gid = 1000, home = "/home/app")],
)

trust_store(
    name = "trust",
    certs = ["//pki:corporate-root.pem"],
)

base_image_layer(
    name = "base_layer",
    srcs = [":skeleton", ":users", ":trust"],
)

image_manifest(
    name = "base",
    layers = [":base_layer"],
)
```

See the [generated API reference](base_images.md) for every rule and attribute.

## Why descriptions instead of layers

Each content rule returns a `BaseImageContentInfo`: two depsets, one of tar
entry metadata and one of the files that metadata points at. No tar is written,
nothing is compressed, and nothing large is copied.

That matters because base image content is naturally composed. A team's
"standard base" is a handful of rules; a product image adds a few more on top; a
test variant swaps one out. If every rule produced a layer, each of those would
be a separate layer in the final image — or would need flattening, which means
writing and re-reading the same bytes. Because the description is just metadata,
it propagates through dependencies for almost nothing, and the bytes get written
exactly once, by `base_image_layer`.

The metadata is a zstd-compressed stream of protobuf messages describing tar
entries. It is an internal format: producer and consumer always ship in the same
binary, so it carries no version and may change at any time.

## Platform scoping

Not every piece of a base image makes sense everywhere. A Linux directory
skeleton is meaningless on Windows; `/etc/passwd` applies to any Unix; a Java
truststore applies everywhere.

Each rule declares which platforms it applies to, and **a rule outside its scope
is a no-op, not an error**. It still analyses and still returns a provider — the
provider is simply empty.

| Scope | Rules |
| --- | --- |
| Any platform | `trust_store` |
| Any Unix | `etc_passwd` |
| Linux only | `linux_skeleton`, `system_libraries`, `etc_environment`, `etc_hosts`, `etc_release` |

This means one BUILD file describes a base image for several platforms without a
`select()` around every rule:

```starlark
base_image_layer(
    name = "base_layer",
    srcs = [
        ":skeleton",  # contributes nothing when targeting Windows
        ":users",     # contributes nothing when targeting Windows
        ":trust",     # contributes everywhere
    ],
)
```

Built for `linux_amd64` this produces the full filesystem. Built for
`windows_amd64` it produces just the trust store.

## Merging and conflicts

`base_image_layer` merges its `srcs` in order. For a path described by more than
one src, **the last one wins**, so a rule can deliberately override something an
earlier one placed:

```starlark
linux_skeleton(name = "skeleton")          # describes /tmp as 1777

linux_skeleton(
    name = "private_tmp",
    etc = "disabled",
    # ... only to override /tmp
    extra_directories = {"/tmp": file_metadata(mode = "0700")},
)

base_image_layer(
    name = "base_layer",
    srcs = [":skeleton", ":private_tmp"],  # /tmp ends up 0700
)
```

Two entries for the same path with *different types* are an error instead — a
directory where another src put a symlink is a rule authoring mistake, not an
override, and silently picking one would produce a subtly broken image. The
error names both rules:

```
conflicting base metadata for "lib": //base:skeleton describes it as symlink,
//base:libs as directory
```

The same goes for an entry placed *underneath* something that is not a
directory. Neither entry is wrong on its own, but a file cannot live inside a
symlink:

```
//base:libs places "lib/libc.so.6" underneath "lib", which //base:skeleton
describes as a symlink rather than a directory
```

In practice that means `usr_merged` disagrees between `linux_skeleton` and
`system_libraries`. Set it the same on both.

## The rules

### `linux_skeleton`

The empty directory structure: `/etc`, `/usr` and its subdirectories, `/var`,
`/run`, `/tmp`, `/root`, `/home`, the kernel mount points, and `/opt` and
`/srv`. Modes follow the Filesystem Hierarchy Standard — `0755` for what the
system owns, `1777` for shared scratch space, `0700` for `/root`, `0555` for the
pseudo-filesystem mount points.

Each group can be turned off individually, and `usr_merged` (on by default)
decides whether `/bin`, `/sbin`, `/lib` and `/lib64` are symlinks into `/usr` or
real directories.

```starlark
# Just enough for a static binary that writes nowhere.
linux_skeleton(
    name = "skeleton",
    home = "disabled",
    opt_srv = "disabled",
    var = "disabled",
)
```

### `etc_passwd`

`/etc/passwd`, `/etc/group`, `/etc/shadow`, and a directory for each user's
home. Entries are built with the `passwd_entry()` and `group_entry()` helpers
and can also be merged in from existing files.

Duplicate names or IDs are an error, not a last-one-wins merge. Shadow entries
are always written locked (`!*`): the rule has no way to accept a password hash,
because anything it wrote would be readable by anyone who can pull the image or
read the Bazel cache.

### `trust_store`

CA certificates, from raw files (PEM, DER or PKCS#7) and from `.deb` / `.rpm`
packages, deduplicated and written in whichever layouts the image needs — a
concatenated PEM bundle, OpenSSL's hashed certificate directory, and/or a
PKCS#12 truststore for the JVM.

Package inputs are typed separately (`debs`, `rpms`) from raw certificates so
the two cannot be confused. Only files under the standard CA certificate
directories are read; no other package metadata is interpreted.

Inputs are parsed strictly — a file that is not a certificate fails the build
rather than being skipped, because an image quietly missing a CA fails much
later and much more confusingly. Packages whose payload is xz-compressed are
rejected with a message saying so; decompressing them would mean a dependency
the core `img` tool deliberately does without.

```starlark
trust_store(
    name = "trust",
    certs = ["//pki:corporate-root.pem"],
    debs = ["@bookworm//ca-certificates/amd64:data"],
    java_keystore = True,
)
```

### `system_libraries`

Shared libraries, placed in the configured library directory with the `SONAME`
symlink the dynamic linker resolves them through. The `SONAME` is read out of
the ELF file, so `libssl.so.3.0.14` automatically gains the `libssl.so.3` a
binary asks for.

`libdir_layout` picks between `/usr/lib` (`plain`), `/usr/lib64` (`lib64`) and
Debian's `/usr/lib/x86_64-linux-gnu` (`multiarch`). An `/etc/ld.so.conf.d`
fragment records the directory, and `ldso_cache = True` writes a prebuilt
`/etc/ld.so.cache` so the loader does not have to scan at startup (glibc only;
musl ignores the file).

The rule takes plain files rather than `CcInfo`, so that rules_img does not have
to depend on `rules_cc`.

### `etc_environment`, `etc_hosts`, `etc_release`

The small text files. Each takes a dict of values and, optionally, existing
files to merge in.

`etc_release` writes the real `os-release` to `/usr/lib/os-release` with
`/etc/os-release` as a relative symlink, which is what
[os-release(5)](https://www.freedesktop.org/software/systemd/man/os-release.html)
specifies so the identity travels with `/usr`. Set `usr_lib_symlink = False` for
a plain file.

All three support [template expansion](templating.md), so values can come from
build settings or from stamping:

```starlark
etc_release(
    name = "release",
    build_settings = {"VERSION": "//settings:version"},
    os_release = {
        "ID": "acme-base",
        "PRETTY_NAME": "ACME Base {{.VERSION}}",
        "VERSION_ID": "{{.VERSION}}",
    },
)
```

## Inspecting the result

`base_image_layer` is an ordinary rules_img layer, so it has the usual output
groups. The `mtree` group is the quickest way to see exactly what came out:

```console
$ bazel build //base:base_layer
$ img mtree --tar bazel-bin/base/base_layer.tgz --output /dev/stdout
```

## Under the hood

Each content rule runs one `img base <verb>` action:

| Rule | Command |
| --- | --- |
| `etc_environment` | `img base etc environment` |
| `etc_hosts` | `img base etc hosts` |
| `etc_release` | `img base etc release` |
| `etc_passwd` | `img base etc passwd` |
| `trust_store` | `img base trust-store` |
| `system_libraries` | `img base system-libraries` |
| `linux_skeleton` | `img base skeleton` |

`base_image_layer` passes every resulting stream to `img layer` via
`--base-metadata`, which merges them and writes the layer through the same code
path as `image_layer` — so compression, eStargz, SOCI, compact streams and
content deduplication all work exactly as they do elsewhere.
