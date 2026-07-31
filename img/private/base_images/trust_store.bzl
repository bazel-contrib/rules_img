"""Rule describing a CA certificate trust store."""

load("//img/private/base_images:common.bzl", "SCOPE_ALL", "base_content_attrs", "empty_content", "in_scope", "merge_sources", "run_base_verb")
load("//img/private/common:build.bzl", "TOOLCHAINS")
load("//img/private/providers:base_image_content_info.bzl", "BaseImageContentInfo")

def _trust_store_impl(ctx):
    # A trust store is meaningful on every platform, so this is never a no-op.
    # The check stays for symmetry with the other content rules.
    if not in_scope(ctx, SCOPE_ALL):
        return empty_content()

    certs = merge_sources(ctx, ctx.attr.certs)
    debs = merge_sources(ctx, ctx.attr.debs)
    rpms = merge_sources(ctx, ctx.attr.rpms)
    if not certs and not debs and not rpms:
        fail("trust_store requires at least one of certs, debs or rpms")

    args = ctx.actions.args()
    args.add_all(certs, before_each = "--cert")
    args.add_all(debs, before_each = "--deb")
    args.add_all(rpms, before_each = "--rpm")

    args.add("--bundle" if ctx.attr.bundle else "--bundle=false")
    args.add("--bundle-path", ctx.attr.bundle_path)
    args.add("--exploded" if ctx.attr.exploded else "--exploded=false")
    args.add("--exploded-dir", ctx.attr.exploded_dir)
    args.add("--java-keystore" if ctx.attr.java_keystore else "--java-keystore=false")
    args.add("--java-keystore-path", ctx.attr.java_keystore_path)
    args.add("--java-keystore-password", ctx.attr.java_keystore_password)
    if ctx.attr.mode:
        args.add("--mode", ctx.attr.mode)

    return run_base_verb(ctx, ["trust-store"], args, inputs = [certs + debs + rpms])

trust_store = rule(
    implementation = _trust_store_impl,
    doc = """Describes a CA certificate trust store.

Certificates are collected from raw files and from distribution packages,
deduplicated, and written in whichever layouts the image needs:

- a single concatenated PEM bundle, which is what `SSL_CERT_FILE` and most TLS
  libraries expect
- an exploded directory of one PEM file per certificate plus the
  `<subject_hash>.0` symlinks OpenSSL resolves `SSL_CERT_DIR` lookups through
- a PKCS#12 truststore for the JVM

Inputs are parsed strictly: a file that is not PEM, DER or PKCS#7 fails the
build rather than being skipped, because a base image quietly missing a CA fails
much later and much more confusingly.

Package inputs are typed separately from raw certificates so the two cannot be
mixed up. Only files under the standard CA certificate directories are read out
of a package; no other package metadata is interpreted. Packages whose payload
is xz-compressed are rejected: decompressing them would mean taking on a
dependency the core `img` tool deliberately does without.

Everything except the exploded tree is platform-independent, so this rule
applies on every target platform.

Example:

```python
load("@rules_img//img:base_images.bzl", "trust_store")

trust_store(
    name = "trust",
    certs = ["//pki:corporate-root.pem"],
    debs = ["@bookworm//ca-certificates/amd64:data"],
    java_keystore = True,
)
```
""",
    attrs = base_content_attrs({
        "certs": attr.label_list(
            doc = """Certificate files, in PEM, DER or PKCS#7 form.

A PEM file may hold any number of certificates. Blocks that are not certificates
(a key or a CRL sitting in the same file) are skipped.""",
            allow_files = True,
        ),
        "debs": attr.label_list(
            doc = """Debian packages to harvest certificates from.

Only files under the standard CA certificate directories are read.""",
            allow_files = True,
        ),
        "rpms": attr.label_list(
            doc = """RPM packages to harvest certificates from.

Only files under the standard CA certificate directories are read.""",
            allow_files = True,
        ),
        "bundle": attr.bool(
            default = True,
            doc = "Whether to write a single concatenated PEM bundle.",
        ),
        "bundle_path": attr.string(
            default = "/etc/ssl/certs/ca-certificates.crt",
            doc = "Path of the PEM bundle inside the image.",
        ),
        "exploded": attr.bool(
            default = True,
            doc = """Whether to write the exploded certificate directory.

This is the layout OpenSSL walks when an application sets `SSL_CERT_DIR` or
relies on the compiled-in default.""",
        ),
        "exploded_dir": attr.string(
            default = "/etc/ssl/certs",
            doc = "Directory of the exploded certificate tree inside the image.",
        ),
        "java_keystore": attr.bool(
            default = False,
            doc = """Whether to write a PKCS#12 truststore for the JVM.

The store holds trusted certificate entries only and contains no private keys,
so the password protects only its integrity check.""",
        ),
        "java_keystore_path": attr.string(
            default = "/etc/ssl/certs/java/cacerts",
            doc = "Path of the PKCS#12 truststore inside the image.",
        ),
        "java_keystore_password": attr.string(
            default = "changeit",
            doc = """Password protecting the truststore's integrity check.

`changeit` is the JDK's own default and what tooling assumes. There is no secret
in the store, so the value is not sensitive.""",
        ),
        "mode": attr.string(
            doc = """Octal mode of the written files, e.g. `"0644"`. Defaults to `0644`.""",
        ),
    }),
    toolchains = TOOLCHAINS,
    provides = [BaseImageContentInfo],
)
