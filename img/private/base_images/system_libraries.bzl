"""Rule describing the shared libraries of a Linux base image."""

load("//img/private/base_images:common.bzl", "SCOPE_LINUX", "base_content_attrs", "empty_content", "in_scope", "merge_sources", "run_base_verb")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/config:defs.bzl", "TargetPlatformInfo")
load("//img/private/providers:base_image_content_info.bzl", "BaseImageContentInfo")

# Debian's multiarch tuple per GOARCH. These are the directory names a Debian or
# Ubuntu image looks for libraries in.
_MULTIARCH_TUPLE = {
    "386": "i386-linux-gnu",
    "amd64": "x86_64-linux-gnu",
    "arm": "arm-linux-gnueabihf",
    "arm64": "aarch64-linux-gnu",
    "mips64": "mips64el-linux-gnuabi64",
    "ppc64le": "powerpc64le-linux-gnu",
    "riscv64": "riscv64-linux-gnu",
    "s390x": "s390x-linux-gnu",
}

# The architectures whose ld.so.cache is read big-endian. Everything else in the
# supported set is little-endian.
_BIG_ENDIAN_ARCH = ["s390x"]

def _library_directory(ctx):
    """Returns the directory shared libraries are placed in."""
    root = "/usr/lib" if ctx.attr.usr_merged else "/lib"
    layout = ctx.attr.libdir_layout
    if layout == "plain":
        return root
    if layout == "lib64":
        # A lib64 layout keeps 64-bit libraries next to a 32-bit /usr/lib, which
        # is what Fedora and its derivatives do.
        return root + "64"
    if layout == "multiarch":
        cpu = ctx.attr._os_cpu[TargetPlatformInfo].cpu
        tuple_name = _MULTIARCH_TUPLE.get(cpu)
        if tuple_name == None:
            fail("libdir_layout = \"multiarch\" does not know a Debian multiarch tuple for {}; use \"plain\" or \"lib64\"".format(cpu))
        return "{}/{}".format(root, tuple_name)
    fail("unknown libdir_layout: {}".format(layout))

def _system_libraries_impl(ctx):
    if not in_scope(ctx, SCOPE_LINUX):
        return empty_content()

    lib_dir = _library_directory(ctx)

    # Keyed by the name the library gets in the image, so a library reached
    # through two different targets (common for a shared transitive dependency)
    # collapses into a single entry rather than a conflict.
    by_name = {}
    for library in merge_sources(ctx, ctx.attr.libs):
        existing = by_name.get(library.basename)
        if existing != None and existing.path != library.path:
            fail("two different files would both be placed at {}/{}: {} and {}".format(
                lib_dir,
                library.basename,
                existing.path,
                library.path,
            ))
        by_name[library.basename] = library

    if not by_name:
        fail("system_libraries requires at least one file in libs")

    args = ctx.actions.args()
    inputs = []
    for name in sorted(by_name):
        library = by_name[name]
        args.add("--library", "{}={}".format(name, library.path))
        inputs.append(library)

    args.add("--lib-dir", lib_dir)
    args.add("--ld-so-conf" if ctx.attr.ldso_conf else "--ld-so-conf=false")
    args.add("--ld-so-cache" if ctx.attr.ldso_cache else "--ld-so-cache=false")
    cpu = ctx.attr._os_cpu[TargetPlatformInfo].cpu
    args.add("--byte-order", "big" if cpu in _BIG_ENDIAN_ARCH else "little")
    if ctx.attr.default_metadata:
        args.add("--default-metadata", ctx.attr.default_metadata)
    for path, metadata in ctx.attr.file_metadata.items():
        args.add("--file-metadata", "{}={}".format(path, metadata))
    if ctx.attr.mode:
        args.add("--mode", ctx.attr.mode)

    return run_base_verb(
        ctx,
        ["system-libraries"],
        args,
        # The libraries are inputs to this action (their ELF headers are read to
        # find each SONAME) and are also referenced by path from the metadata,
        # so the layer action needs them too.
        inputs = [inputs],
        referenced_files = [depset(inputs)],
    )

system_libraries = rule(
    implementation = _system_libraries_impl,
    doc = """Describes the shared libraries of a Linux base image.

Each library is placed in the configured library directory and given the
symlink named after its `SONAME`, which is the name the dynamic linker actually
resolves a dependency through. The `SONAME` is read out of the ELF file itself,
so a library named `libssl.so.3.0.14` automatically gains the `libssl.so.3` the
loader looks for.

An `/etc/ld.so.conf.d` fragment records the library directory, and a prebuilt
`/etc/ld.so.cache` can be generated so the loader does not have to scan at
startup.

Every file in `libs` must be an ELF shared object; anything else fails the
build. To pull the shared libraries out of a `cc_library`, name its output files
directly, or use a `filegroup` with the relevant output group -- this rule takes
plain files rather than `CcInfo` so that rules_img does not have to depend on
`rules_cc`.

This rule only applies when targeting Linux. On any other platform it is a
no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "system_libraries")

system_libraries(
    name = "libs",
    libs = [
        "@sysroot//:lib/libssl.so.3.0.14",
        "@sysroot//:lib/libcrypto.so.3.0.14",
    ],
    libdir_layout = "multiarch",
)
```
""",
    attrs = base_content_attrs({
        "libs": attr.label_list(
            doc = """Shared library files to place in the image.

Every file must be an ELF shared object. Two targets contributing the same
library are fine; two different files that would land at the same name are an
error.""",
            allow_files = True,
        ),
        "usr_merged": attr.bool(
            default = True,
            doc = """Whether the image uses the merged-`/usr` layout.

When True, libraries go under `/usr/lib`; when False, under `/lib`.

Set the same value on `linux_skeleton`, which is what creates the
`/lib -> usr/lib` symlink. Mismatching them fails the build: placing a library
at `/lib/...` when the skeleton made `/lib` a symlink would produce an image
whose libraries are unreachable.""",
        ),
        "libdir_layout": attr.string(
            default = "plain",
            values = ["plain", "lib64", "multiarch"],
            doc = """How the library directory is named.

- **`plain`** (default): `/usr/lib`.
- **`lib64`**: `/usr/lib64`, as Fedora and its derivatives use.
- **`multiarch`**: `/usr/lib/<tuple>` with the Debian multiarch tuple for the
  target architecture, e.g. `/usr/lib/x86_64-linux-gnu`.""",
        ),
        "ldso_conf": attr.bool(
            default = True,
            doc = "Whether to write an `/etc/ld.so.conf.d` fragment naming the library directory.",
        ),
        "ldso_cache": attr.bool(
            default = False,
            doc = """Whether to write a prebuilt `/etc/ld.so.cache`.

The cache is glibc-specific; musl ignores the file entirely. It saves the loader
a directory scan at startup, at the cost of a file that must be regenerated
whenever the library set changes -- which is why it is off by default.""",
        ),
        "default_metadata": attr.string(
            doc = """JSON-encoded metadata applied to every placed library.

Build it with `file_metadata()` from `@rules_img//img:layer.bzl`.""",
        ),
        "file_metadata": attr.string_dict(
            doc = """Per-file metadata overrides, mapping image path to JSON-encoded metadata.

The path must be the library's full path in the image, including the library
directory.""",
        ),
        "mode": attr.string(
            doc = """Octal mode of the placed libraries, e.g. `"0755"`. Defaults to `0755`.""",
        ),
    }),
    toolchains = TOOLCHAINS,
    provides = [BaseImageContentInfo],
)
