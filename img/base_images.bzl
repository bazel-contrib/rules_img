"""Public API for building bespoke container base images.

These rules describe the contents of a base image -- the directory skeleton,
users and groups, the CA trust store, shared libraries, the standard files under
`/etc` -- without building a layer. Each returns a `BaseImageContentInfo`
carrying nothing but tar entry metadata (plus the files that metadata points
at), so a description costs almost nothing to propagate through dependencies.

`base_image_layer` is what finally materializes them: it takes any number of
descriptions and merges them into a single flat layer.

Rules are scoped to the platforms they make sense on. `linux_skeleton` is
Linux-only, `etc_passwd` applies to any Unix, and `trust_store` applies
everywhere. A rule outside its scope is a no-op rather than an error, so one
BUILD file can describe a base image for several platforms without a `select()`
around every rule.

```python
load(
    "@rules_img//img:base_images.bzl",
    "base_image_layer",
    "etc_passwd",
    "linux_skeleton",
    "passwd_entry",
    "trust_store",
)

linux_skeleton(name = "skeleton")

etc_passwd(
    name = "users",
    users = [passwd_entry(username = "app", uid = 1000, gid = 1000, home = "/home/app")],
)

trust_store(
    name = "trust",
    debs = ["@bookworm//ca-certificates/amd64:data"],
)

base_image_layer(
    name = "base_layer",
    srcs = [":skeleton", ":users", ":trust"],
)
```
"""

load("//img/private/base_images:base_image_layer.bzl", _base_image_layer = "base_image_layer")
load(
    "//img/private/base_images:etc.bzl",
    _etc_environment = "etc_environment",
    _etc_hosts = "etc_hosts",
    _etc_passwd = "etc_passwd",
    _etc_release = "etc_release",
    _group_entry = "group_entry",
    _passwd_entry = "passwd_entry",
)
load("//img/private/base_images:linux_skeleton.bzl", _linux_skeleton = "linux_skeleton")
load("//img/private/base_images:system_libraries.bzl", _system_libraries = "system_libraries")
load("//img/private/base_images:trust_store.bzl", _trust_store = "trust_store")

# The terminal rule: turns content descriptions into a layer.
base_image_layer = _base_image_layer

# Content rules, Linux-only.
etc_environment = _etc_environment
etc_hosts = _etc_hosts
etc_release = _etc_release
linux_skeleton = _linux_skeleton
system_libraries = _system_libraries

# Content rules, any Unix.
etc_passwd = _etc_passwd

# Content rules, any platform.
trust_store = _trust_store

# Helpers for building the entries of etc_passwd.
passwd_entry = _passwd_entry
group_entry = _group_entry
