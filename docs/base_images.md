<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for building bespoke container base images.

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

<a id="base_image_layer"></a>

## base_image_layer

<pre>
load("@rules_img//img:base_images.bzl", "base_image_layer")

base_image_layer(<a href="#base_image_layer-name">name</a>, <a href="#base_image_layer-srcs">srcs</a>, <a href="#base_image_layer-annotations">annotations</a>, <a href="#base_image_layer-annotations_file">annotations_file</a>, <a href="#base_image_layer-compress">compress</a>, <a href="#base_image_layer-create_parent_directories">create_parent_directories</a>,
                 <a href="#base_image_layer-default_metadata">default_metadata</a>, <a href="#base_image_layer-estargz">estargz</a>, <a href="#base_image_layer-file_metadata">file_metadata</a>, <a href="#base_image_layer-include_runfiles">include_runfiles</a>, <a href="#base_image_layer-media_type">media_type</a>, <a href="#base_image_layer-soci">soci</a>,
                 <a href="#base_image_layer-tree_artifact_handling">tree_artifact_handling</a>)
</pre>

Builds a single flat container image layer from base image content descriptions.

Takes any number of content-describing targets -- `linux_skeleton`, `etc_passwd`,
`trust_store` and the rest -- and merges everything they describe into one
layer. Nothing is materialized until this point: the content rules only
accumulate tar entry metadata, which propagates cheaply through dependencies.

For a path described by more than one `src`, the last one wins, so a rule can
override what an earlier one placed. Two entries for the same path with
different *types* (a directory where another src put a symlink) are an error
instead: that is a rule authoring mistake rather than a deliberate override.

The resulting layer is an ordinary rules_img layer and supports the same
compression, eStargz, SOCI and compact-stream settings as `image_layer`.

Example:

```python
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
    srcs = [
        ":skeleton",
        ":users",
        ":trust",
    ],
)

image_manifest(
    name = "base",
    layers = [":base_layer"],
)
```

### Output groups

- `layer`: the compressed layer blob
- `metadata`: the layer's OCI descriptor as JSON
- `mtree`: a single [mtree](https://man.freebsd.org/cgi/man.cgi?mtree(5)) text file

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="base_image_layer-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="base_image_layer-srcs"></a>srcs |  Base image content descriptions to merge into the layer.<br><br>Order matters: for a path described by more than one src, the last one wins.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="base_image_layer-annotations"></a>annotations |  Annotations to add to the layer metadata as key-value pairs.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="base_image_layer-annotations_file"></a>annotations_file |  File containing annotations for the layer, as JSON or newline-delimited text.<br><br>The file is parsed in one of the following formats, auto-detected from its contents:<br><br>- JSON object with string values: `{"key": "value"}` - JSON object with list values: `{"key": ["value1", "value2"]}` (the last value wins) - JSON array of `KEY=VALUE` strings: `["key=value"]` - newline-delimited `KEY=VALUE` text (one per line; blank lines and `#` comments are ignored)<br><br>Values in JSON objects are used verbatim, so they can encode arbitrary strings including values that contain `=`, spaces, or newlines. The `KEY=VALUE` forms (JSON array and text) split on the first `=` and trim surrounding whitespace from the key and value.<br><br>Annotations from this file are merged with annotations specified via the `annotations` attribute, which take precedence for matching keys.<br><br>Example file content: <pre><code>version=1.0.0&#10;build.date=2024-01-15&#10;source.url=https://github.com/...</code></pre>   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="base_image_layer-compress"></a>compress |  Compression algorithm to use. If set to 'auto', uses the global default compression setting.   | String | optional |  `"auto"`  |
| <a id="base_image_layer-create_parent_directories"></a>create_parent_directories |  Whether to automatically create parent directory entries in the tar file for all files. If set to 'auto', uses the global default create_parent_directories setting. When enabled, parent directories will be created automatically for all files in the layer.   | String | optional |  `"auto"`  |
| <a id="base_image_layer-default_metadata"></a>default_metadata |  JSON-encoded default metadata for entries that do not set it themselves.<br><br>Only the modification time is taken from here: an entry's own mode and ownership are authoritative, since deciding them is the entire purpose of the rule that produced it. Build the value with `file_metadata()` from `@rules_img//img:layer.bzl`.   | String | optional |  `""`  |
| <a id="base_image_layer-estargz"></a>estargz |  Whether to use estargz format. If set to 'auto', uses the global default estargz setting. When enabled, the layer will be optimized for lazy pulling and will be compatible with the estargz format.   | String | optional |  `"auto"`  |
| <a id="base_image_layer-file_metadata"></a>file_metadata |  Per-file metadata overrides, mapping image path to JSON-encoded metadata.<br><br>Unlike `default_metadata`, an override here replaces whatever the producing rule chose. Use it to adjust a single entry without changing the rule that describes it.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="base_image_layer-include_runfiles"></a>include_runfiles |  Whether to include runfiles for executable targets. When True (default), executables in srcs will include their runfiles tree. When False, only the executable file itself is included, without runfiles.<br><br>Either way, any additional default outputs of the target (the rest of `DefaultInfo.files` beyond the executable) are copied into the layer, placed relative to the executable.   | Boolean | optional |  `True`  |
| <a id="base_image_layer-media_type"></a>media_type |  Override the layer media type. By default, the media type is auto-detected from the compression algorithm.   | String | optional |  `""`  |
| <a id="base_image_layer-soci"></a>soci |  Whether to emit a SOCI ztoc (table of contents) for this layer. If set to 'auto', uses the global default //img/settings:soci setting. When enabled and the layer is gzip-compressed, a ztoc is produced in the layer action and recorded on the SingleLayerInfo provider, so images that build a SOCI Index Manifest v2 can reuse it instead of regenerating it. Non-gzip layers never emit a ztoc.   | String | optional |  `"auto"`  |
| <a id="base_image_layer-tree_artifact_handling"></a>tree_artifact_handling |  How to handle duplicate tree artifacts (directories) in the layer. If set to 'full', each tree artifact is stored at its intended path (no deduplication). If set to 'deduplicate_symlink', duplicate tree artifacts are replaced with symlinks to the first occurrence. If set to 'auto', uses the global default from --@rules_img//img/settings:layer_tree_artifact_handling.   | String | optional |  `"auto"`  |


<a id="etc_environment"></a>

## etc_environment

<pre>
load("@rules_img//img:base_images.bzl", "etc_environment")

etc_environment(<a href="#etc_environment-name">name</a>, <a href="#etc_environment-srcs">srcs</a>, <a href="#etc_environment-build_settings">build_settings</a>, <a href="#etc_environment-env">env</a>, <a href="#etc_environment-mode">mode</a>, <a href="#etc_environment-path">path</a>, <a href="#etc_environment-quote">quote</a>, <a href="#etc_environment-stamp">stamp</a>)
</pre>

Describes `/etc/environment`, the system-wide environment file read by PAM.

The file holds one `KEY="value"` assignment per line. Unlike a shell profile it
is not executed, so values are literal: no expansion, no `export`, no
conditionals.

This rule only applies when targeting Linux. On any other platform it is a
no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "etc_environment")

etc_environment(
    name = "environment",
    env = {
        "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
        "LANG": "C.UTF-8",
    },
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="etc_environment-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="etc_environment-srcs"></a>srcs |  Existing environment files to merge in.<br><br>Files are read in order and later files win. Anything set via `env` overrides all of them.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="etc_environment-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to `string_flag` targets. The values can be referenced from this rule's templated attributes with `{{.VARIABLE_NAME}}` (Go template syntax).<br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="etc_environment-env"></a>env |  Environment variables to write, as a mapping of name to value.<br><br>Values support [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="etc_environment-mode"></a>mode |  Octal file mode, e.g. `"0644"`. Defaults to `0644`.   | String | optional |  `""`  |
| <a id="etc_environment-path"></a>path |  Path of the file inside the image.   | String | optional |  `"/etc/environment"`  |
| <a id="etc_environment-quote"></a>quote |  Whether to wrap values in double quotes.<br><br>Quoting is conventional and is what a distribution ships. Turn it off only if something in the image parses the file naively.   | Boolean | optional |  `True`  |
| <a id="etc_environment-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |


<a id="etc_hosts"></a>

## etc_hosts

<pre>
load("@rules_img//img:base_images.bzl", "etc_hosts")

etc_hosts(<a href="#etc_hosts-name">name</a>, <a href="#etc_hosts-srcs">srcs</a>, <a href="#etc_hosts-build_settings">build_settings</a>, <a href="#etc_hosts-hosts">hosts</a>, <a href="#etc_hosts-include_defaults">include_defaults</a>, <a href="#etc_hosts-mode">mode</a>, <a href="#etc_hosts-path">path</a>, <a href="#etc_hosts-stamp">stamp</a>)
</pre>

Describes `/etc/hosts`, the static hostname table.

Names accumulate per address rather than replacing each other, so the default
loopback entries and your own names for `127.0.0.1` end up on the same line.

This rule only applies when targeting Linux. On any other platform it is a
no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "etc_hosts")

etc_hosts(
    name = "hosts",
    hosts = {
        "10.0.0.7": "database db.internal",
        "127.0.0.1": "myapp.local",
    },
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="etc_hosts-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="etc_hosts-srcs"></a>srcs |  Existing hosts files to merge in, read in order.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="etc_hosts-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to `string_flag` targets. The values can be referenced from this rule's templated attributes with `{{.VARIABLE_NAME}}` (Go template syntax).<br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="etc_hosts-hosts"></a>hosts |  Host mappings, as a mapping of IP address to space-separated host names.<br><br>Values support [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="etc_hosts-include_defaults"></a>include_defaults |  Whether to include the standard loopback entries.<br><br>These are the `127.0.0.1 localhost` and IPv6 `::1`/`ff02::` entries that `netbase` ships. Without them, resolving `localhost` inside the container depends on the resolver's own fallbacks.   | Boolean | optional |  `True`  |
| <a id="etc_hosts-mode"></a>mode |  Octal file mode, e.g. `"0644"`. Defaults to `0644`.   | String | optional |  `""`  |
| <a id="etc_hosts-path"></a>path |  Path of the file inside the image.   | String | optional |  `"/etc/hosts"`  |
| <a id="etc_hosts-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |


<a id="etc_passwd"></a>

## etc_passwd

<pre>
load("@rules_img//img:base_images.bzl", "etc_passwd")

etc_passwd(<a href="#etc_passwd-name">name</a>, <a href="#etc_passwd-build_settings">build_settings</a>, <a href="#etc_passwd-create_home_directories">create_home_directories</a>, <a href="#etc_passwd-group_path">group_path</a>, <a href="#etc_passwd-group_srcs">group_srcs</a>, <a href="#etc_passwd-groups">groups</a>,
           <a href="#etc_passwd-home_directory_mode">home_directory_mode</a>, <a href="#etc_passwd-mode">mode</a>, <a href="#etc_passwd-passwd_path">passwd_path</a>, <a href="#etc_passwd-passwd_srcs">passwd_srcs</a>, <a href="#etc_passwd-shadow_mode">shadow_mode</a>, <a href="#etc_passwd-shadow_path">shadow_path</a>, <a href="#etc_passwd-shadow_srcs">shadow_srcs</a>,
           <a href="#etc_passwd-stamp">stamp</a>, <a href="#etc_passwd-users">users</a>, <a href="#etc_passwd-write_shadow">write_shadow</a>)
</pre>

Describes `/etc/passwd`, `/etc/group`, `/etc/shadow` and home directories.

Users and groups are declared with the `passwd_entry()` and `group_entry()`
helpers, and may also be merged in from existing files. Duplicate user names,
UIDs, group names or GIDs are an error rather than a last-one-wins merge.

Shadow entries are always written locked (`!*`). This rule never accepts a
password hash: anything it wrote would be readable by anyone who can pull the
image or read the Bazel cache.

This rule applies when targeting any Unix-like platform (Linux, macOS, the
BSDs). On Windows it is a no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "etc_passwd", "group_entry", "passwd_entry")

etc_passwd(
    name = "users",
    users = [
        passwd_entry(username = "root", uid = 0, gid = 0, home = "/root", shell = "/bin/sh"),
        passwd_entry(username = "app", uid = 1000, gid = 1000, home = "/home/app"),
    ],
    groups = [
        group_entry(name = "root", gid = 0),
        group_entry(name = "app", gid = 1000, users = ["app"]),
    ],
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="etc_passwd-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="etc_passwd-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to `string_flag` targets. The values can be referenced from this rule's templated attributes with `{{.VARIABLE_NAME}}` (Go template syntax).<br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="etc_passwd-create_home_directories"></a>create_home_directories |  Whether to create a directory for each user's home.<br><br>Users whose home is `/` (the convention for a system account without one) are skipped. Root's home is always created with mode `0700`.   | Boolean | optional |  `True`  |
| <a id="etc_passwd-group_path"></a>group_path |  Path of the group file inside the image.   | String | optional |  `"/etc/group"`  |
| <a id="etc_passwd-group_srcs"></a>group_srcs |  Existing group files to merge in.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="etc_passwd-groups"></a>groups |  Groups to define, as JSON strings built by `group_entry()`.<br><br>Values support [template expansion](/docs/templating.md).   | List of strings | optional |  `[]`  |
| <a id="etc_passwd-home_directory_mode"></a>home_directory_mode |  Octal mode of created home directories.   | String | optional |  `"0750"`  |
| <a id="etc_passwd-mode"></a>mode |  Octal mode of passwd and group, e.g. `"0644"`. Defaults to `0644`.   | String | optional |  `""`  |
| <a id="etc_passwd-passwd_path"></a>passwd_path |  Path of the passwd file inside the image.   | String | optional |  `"/etc/passwd"`  |
| <a id="etc_passwd-passwd_srcs"></a>passwd_srcs |  Existing passwd files to merge in.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="etc_passwd-shadow_mode"></a>shadow_mode |  Octal mode of the shadow file. Defaults to `0640`.   | String | optional |  `""`  |
| <a id="etc_passwd-shadow_path"></a>shadow_path |  Path of the shadow file inside the image.   | String | optional |  `"/etc/shadow"`  |
| <a id="etc_passwd-shadow_srcs"></a>shadow_srcs |  Existing shadow files to merge in.<br><br>Records found here are carried over verbatim, ageing fields included. Users with no record get a locked entry.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="etc_passwd-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |
| <a id="etc_passwd-users"></a>users |  Users to define, as JSON strings built by `passwd_entry()`.<br><br>Values support [template expansion](/docs/templating.md).   | List of strings | optional |  `[]`  |
| <a id="etc_passwd-write_shadow"></a>write_shadow |  Whether to write `/etc/shadow` with a locked entry per user.   | Boolean | optional |  `True`  |


<a id="etc_release"></a>

## etc_release

<pre>
load("@rules_img//img:base_images.bzl", "etc_release")

etc_release(<a href="#etc_release-name">name</a>, <a href="#etc_release-build_settings">build_settings</a>, <a href="#etc_release-lsb_release">lsb_release</a>, <a href="#etc_release-lsb_release_path">lsb_release_path</a>, <a href="#etc_release-lsb_release_srcs">lsb_release_srcs</a>, <a href="#etc_release-mode">mode</a>, <a href="#etc_release-os_release">os_release</a>,
            <a href="#etc_release-os_release_path">os_release_path</a>, <a href="#etc_release-os_release_srcs">os_release_srcs</a>, <a href="#etc_release-stamp">stamp</a>, <a href="#etc_release-usr_lib_path">usr_lib_path</a>, <a href="#etc_release-usr_lib_symlink">usr_lib_symlink</a>, <a href="#etc_release-write_lsb_release">write_lsb_release</a>)
</pre>

Describes `/etc/os-release` (and optionally `/etc/lsb-release`).

`os-release` is the standard way for software in the image to identify the
distribution it is running on. Values are always written double-quoted, which is
valid for every field.

By default the real file is written to `/usr/lib/os-release` with
`/etc/os-release` as a relative symlink to it, which is what
[os-release(5)](https://www.freedesktop.org/software/systemd/man/os-release.html)
specifies so the identity travels with `/usr`.

This rule only applies when targeting Linux. On any other platform it is a
no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "etc_release")

etc_release(
    name = "release",
    os_release = {
        "ID": "acme-base",
        "NAME": "ACME Base Image",
        "PRETTY_NAME": "ACME Base Image {{.VERSION}}",
        "VERSION_ID": "{{.VERSION}}",
    },
    build_settings = {"VERSION": "//settings:version"},
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="etc_release-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="etc_release-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to `string_flag` targets. The values can be referenced from this rule's templated attributes with `{{.VARIABLE_NAME}}` (Go template syntax).<br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="etc_release-lsb_release"></a>lsb_release |  `lsb-release` entries, as a mapping of key to value.<br><br>Only written when `write_lsb_release` is True. Values support [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="etc_release-lsb_release_path"></a>lsb_release_path |  Path of the lsb-release file inside the image.   | String | optional |  `"/etc/lsb-release"`  |
| <a id="etc_release-lsb_release_srcs"></a>lsb_release_srcs |  Existing lsb-release files to merge in, read in order.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="etc_release-mode"></a>mode |  Octal file mode, e.g. `"0644"`. Defaults to `0644`.   | String | optional |  `""`  |
| <a id="etc_release-os_release"></a>os_release |  `os-release` entries, as a mapping of key to value.<br><br>Values support [template expansion](/docs/templating.md).   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="etc_release-os_release_path"></a>os_release_path |  Path of the os-release file (or its symlink) inside the image.   | String | optional |  `"/etc/os-release"`  |
| <a id="etc_release-os_release_srcs"></a>os_release_srcs |  Existing os-release files to merge in, read in order.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="etc_release-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |
| <a id="etc_release-usr_lib_path"></a>usr_lib_path |  Path of the real os-release file when `usr_lib_symlink` is True.   | String | optional |  `"/usr/lib/os-release"`  |
| <a id="etc_release-usr_lib_symlink"></a>usr_lib_symlink |  Whether to write the real file under `/usr/lib` and make `/etc/os-release` a symlink.<br><br>Set to False to write a plain file at `os_release_path` instead.   | Boolean | optional |  `True`  |
| <a id="etc_release-write_lsb_release"></a>write_lsb_release |  Whether to also write the older LSB release file.<br><br>Most images do not need it; `os-release` is what current software reads.   | Boolean | optional |  `False`  |


<a id="linux_skeleton"></a>

## linux_skeleton

<pre>
load("@rules_img//img:base_images.bzl", "linux_skeleton")

linux_skeleton(<a href="#linux_skeleton-name">name</a>, <a href="#linux_skeleton-bin_and_lib">bin_and_lib</a>, <a href="#linux_skeleton-build_settings">build_settings</a>, <a href="#linux_skeleton-etc">etc</a>, <a href="#linux_skeleton-extra_directories">extra_directories</a>, <a href="#linux_skeleton-home">home</a>, <a href="#linux_skeleton-mount_points">mount_points</a>,
               <a href="#linux_skeleton-opt_srv">opt_srv</a>, <a href="#linux_skeleton-root">root</a>, <a href="#linux_skeleton-run">run</a>, <a href="#linux_skeleton-stamp">stamp</a>, <a href="#linux_skeleton-tmp">tmp</a>, <a href="#linux_skeleton-usr_merged">usr_merged</a>, <a href="#linux_skeleton-var">var</a>)
</pre>

Describes the empty directory skeleton of a Linux base image.

The defaults are a working Filesystem Hierarchy Standard layout with the modes a
stock distribution uses: `0755` for anything the system owns, `1777` for the
shared scratch directories, `0700` for `/root`, and `0555` for the kernel
pseudo-filesystem mount points. Each group of directories can be turned off
individually.

This rule only applies when targeting Linux. On any other platform it is a
no-op that contributes nothing to the layer.

Example:

```python
load("@rules_img//img:base_images.bzl", "linux_skeleton")

# A minimal skeleton for a static binary that needs nowhere to write.
linux_skeleton(
    name = "skeleton",
    home = "disabled",
    opt_srv = "disabled",
    var = "disabled",
)
```

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="linux_skeleton-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="linux_skeleton-bin_and_lib"></a>bin_and_lib |  Whether to create the binary and library directories.<br><br>Covers `/usr` and its subdirectories, plus the top-level `/bin`, `/sbin`, `/lib` and `/lib64` (as symlinks or real directories, per `usr_merged`).   | String | optional |  `"auto"`  |
| <a id="linux_skeleton-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to `string_flag` targets. The values can be referenced from this rule's templated attributes with `{{.VARIABLE_NAME}}` (Go template syntax).<br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="linux_skeleton-etc"></a>etc |  Whether to create `/etc`.   | String | optional |  `"auto"`  |
| <a id="linux_skeleton-extra_directories"></a>extra_directories |  Additional directories to create, mapping path to JSON-encoded metadata.<br><br>Build the metadata with `file_metadata()` from `@rules_img//img:layer.bzl`. An empty value uses mode `0755`, owned by root.<br><br><pre><code class="language-python">extra_directories = {&#10;    "/var/lib/myapp": file_metadata(mode = "0750", uid = 1000, gid = 1000),&#10;}</code></pre>   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="linux_skeleton-home"></a>home |  Whether to create `/home`.   | String | optional |  `"auto"`  |
| <a id="linux_skeleton-mount_points"></a>mount_points |  Whether to create the standard mount points.<br><br>Covers `/dev`, `/proc` and `/sys` (the kernel pseudo-filesystems a runtime mounts over) plus `/mnt` and `/media`.   | String | optional |  `"auto"`  |
| <a id="linux_skeleton-opt_srv"></a>opt_srv |  Whether to create `/opt` and `/srv`.   | String | optional |  `"auto"`  |
| <a id="linux_skeleton-root"></a>root |  Whether to create `/root`, with mode `0700`.   | String | optional |  `"auto"`  |
| <a id="linux_skeleton-run"></a>run |  Whether to create `/run` and `/run/lock`.   | String | optional |  `"auto"`  |
| <a id="linux_skeleton-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |
| <a id="linux_skeleton-tmp"></a>tmp |  Whether to create `/tmp`, with the sticky mode `1777`.   | String | optional |  `"auto"`  |
| <a id="linux_skeleton-usr_merged"></a>usr_merged |  Whether to use the merged-`/usr` layout.<br><br>When True, `/bin`, `/sbin`, `/lib` and `/lib64` are symlinks into `/usr`, as every current distribution does. When False they are real directories.<br><br>Set the same value on `system_libraries`, which places libraries accordingly. Mismatching them fails the build rather than producing an image whose libraries sit inside a symlink and cannot be found.   | Boolean | optional |  `True`  |
| <a id="linux_skeleton-var"></a>var |  Whether to create `/var` and its standard subdirectories.<br><br>Covers `/var/log`, `/var/tmp` (sticky), `/var/cache`, `/var/lib` and `/var/spool`. When the `run` group is enabled too, `/var/run` and `/var/lock` are added as symlinks into `/run`.   | String | optional |  `"auto"`  |


<a id="system_libraries"></a>

## system_libraries

<pre>
load("@rules_img//img:base_images.bzl", "system_libraries")

system_libraries(<a href="#system_libraries-name">name</a>, <a href="#system_libraries-build_settings">build_settings</a>, <a href="#system_libraries-default_metadata">default_metadata</a>, <a href="#system_libraries-file_metadata">file_metadata</a>, <a href="#system_libraries-ldso_cache">ldso_cache</a>, <a href="#system_libraries-ldso_conf">ldso_conf</a>,
                 <a href="#system_libraries-libdir_layout">libdir_layout</a>, <a href="#system_libraries-libs">libs</a>, <a href="#system_libraries-mode">mode</a>, <a href="#system_libraries-stamp">stamp</a>, <a href="#system_libraries-usr_merged">usr_merged</a>)
</pre>

Describes the shared libraries of a Linux base image.

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

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="system_libraries-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="system_libraries-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to `string_flag` targets. The values can be referenced from this rule's templated attributes with `{{.VARIABLE_NAME}}` (Go template syntax).<br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="system_libraries-default_metadata"></a>default_metadata |  JSON-encoded metadata applied to every placed library.<br><br>Build it with `file_metadata()` from `@rules_img//img:layer.bzl`.   | String | optional |  `""`  |
| <a id="system_libraries-file_metadata"></a>file_metadata |  Per-file metadata overrides, mapping image path to JSON-encoded metadata.<br><br>The path must be the library's full path in the image, including the library directory.   | <a href="https://bazel.build/rules/lib/core/dict">Dictionary: String -> String</a> | optional |  `{}`  |
| <a id="system_libraries-ldso_cache"></a>ldso_cache |  Whether to write a prebuilt `/etc/ld.so.cache`.<br><br>The cache is glibc-specific; musl ignores the file entirely. It saves the loader a directory scan at startup, at the cost of a file that must be regenerated whenever the library set changes -- which is why it is off by default.   | Boolean | optional |  `False`  |
| <a id="system_libraries-ldso_conf"></a>ldso_conf |  Whether to write an `/etc/ld.so.conf.d` fragment naming the library directory.   | Boolean | optional |  `True`  |
| <a id="system_libraries-libdir_layout"></a>libdir_layout |  How the library directory is named.<br><br>- **`plain`** (default): `/usr/lib`. - **`lib64`**: `/usr/lib64`, as Fedora and its derivatives use. - **`multiarch`**: `/usr/lib/<tuple>` with the Debian multiarch tuple for the   target architecture, e.g. `/usr/lib/x86_64-linux-gnu`.   | String | optional |  `"plain"`  |
| <a id="system_libraries-libs"></a>libs |  Shared library files to place in the image.<br><br>Every file must be an ELF shared object. Two targets contributing the same library are fine; two different files that would land at the same name are an error.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="system_libraries-mode"></a>mode |  Octal mode of the placed libraries, e.g. `"0755"`. Defaults to `0755`.   | String | optional |  `""`  |
| <a id="system_libraries-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |
| <a id="system_libraries-usr_merged"></a>usr_merged |  Whether the image uses the merged-`/usr` layout.<br><br>When True, libraries go under `/usr/lib`; when False, under `/lib`.<br><br>Set the same value on `linux_skeleton`, which is what creates the `/lib -> usr/lib` symlink. Mismatching them fails the build: placing a library at `/lib/...` when the skeleton made `/lib` a symlink would produce an image whose libraries are unreachable.   | Boolean | optional |  `True`  |


<a id="trust_store"></a>

## trust_store

<pre>
load("@rules_img//img:base_images.bzl", "trust_store")

trust_store(<a href="#trust_store-name">name</a>, <a href="#trust_store-build_settings">build_settings</a>, <a href="#trust_store-bundle">bundle</a>, <a href="#trust_store-bundle_path">bundle_path</a>, <a href="#trust_store-certs">certs</a>, <a href="#trust_store-debs">debs</a>, <a href="#trust_store-exploded">exploded</a>, <a href="#trust_store-exploded_dir">exploded_dir</a>,
            <a href="#trust_store-java_keystore">java_keystore</a>, <a href="#trust_store-java_keystore_password">java_keystore_password</a>, <a href="#trust_store-java_keystore_path">java_keystore_path</a>, <a href="#trust_store-mode">mode</a>, <a href="#trust_store-rpms">rpms</a>, <a href="#trust_store-stamp">stamp</a>)
</pre>

Describes a CA certificate trust store.

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

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="trust_store-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="trust_store-build_settings"></a>build_settings |  Build settings for template expansion.<br><br>Maps template variable names to `string_flag` targets. The values can be referenced from this rule's templated attributes with `{{.VARIABLE_NAME}}` (Go template syntax).<br><br>See [template expansion](/docs/templating.md) for more details.   | Dictionary: String -> Label | optional |  `{}`  |
| <a id="trust_store-bundle"></a>bundle |  Whether to write a single concatenated PEM bundle.   | Boolean | optional |  `True`  |
| <a id="trust_store-bundle_path"></a>bundle_path |  Path of the PEM bundle inside the image.   | String | optional |  `"/etc/ssl/certs/ca-certificates.crt"`  |
| <a id="trust_store-certs"></a>certs |  Certificate files, in PEM, DER or PKCS#7 form.<br><br>A PEM file may hold any number of certificates. Blocks that are not certificates (a key or a CRL sitting in the same file) are skipped.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="trust_store-debs"></a>debs |  Debian packages to harvest certificates from.<br><br>Only files under the standard CA certificate directories are read.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="trust_store-exploded"></a>exploded |  Whether to write the exploded certificate directory.<br><br>This is the layout OpenSSL walks when an application sets `SSL_CERT_DIR` or relies on the compiled-in default.   | Boolean | optional |  `True`  |
| <a id="trust_store-exploded_dir"></a>exploded_dir |  Directory of the exploded certificate tree inside the image.   | String | optional |  `"/etc/ssl/certs"`  |
| <a id="trust_store-java_keystore"></a>java_keystore |  Whether to write a PKCS#12 truststore for the JVM.<br><br>The store holds trusted certificate entries only and contains no private keys, so the password protects only its integrity check.   | Boolean | optional |  `False`  |
| <a id="trust_store-java_keystore_password"></a>java_keystore_password |  Password protecting the truststore's integrity check.<br><br>`changeit` is the JDK's own default and what tooling assumes. There is no secret in the store, so the value is not sensitive.   | String | optional |  `"changeit"`  |
| <a id="trust_store-java_keystore_path"></a>java_keystore_path |  Path of the PKCS#12 truststore inside the image.   | String | optional |  `"/etc/ssl/certs/java/cacerts"`  |
| <a id="trust_store-mode"></a>mode |  Octal mode of the written files, e.g. `"0644"`. Defaults to `0644`.   | String | optional |  `""`  |
| <a id="trust_store-rpms"></a>rpms |  RPM packages to harvest certificates from.<br><br>Only files under the standard CA certificate directories are read.   | <a href="https://bazel.build/concepts/labels">List of labels</a> | optional |  `[]`  |
| <a id="trust_store-stamp"></a>stamp |  Controls build stamping for template expansion.<br><br>- **`auto`** (default): Defers to the global `--@rules_img//img/settings:stamp` setting. - **`force`**: Always stamp if templates contain `{{}}` placeholders, ignoring Bazel's `--stamp` flag. - **`disabled`**: Never include stamp information.<br><br>See [template expansion](/docs/templating.md) for available stamp variables.   | String | optional |  `"auto"`  |


<a id="group_entry"></a>

## group_entry

<pre>
load("@rules_img//img:base_images.bzl", "group_entry")

group_entry(*, <a href="#group_entry-name">name</a>, <a href="#group_entry-gid">gid</a>, <a href="#group_entry-users">users</a>)
</pre>

Describes one group for the `groups` attribute of `etc_passwd`.

**PARAMETERS**


| Name  | Description | Default Value |
| :------------- | :------------- | :------------- |
| <a id="group_entry-name"></a>name |  Group name. String.   |  none |
| <a id="group_entry-gid"></a>gid |  Numeric group ID. Integer.   |  none |
| <a id="group_entry-users"></a>users |  Names of users who are members of this group beyond the ones that have it as their primary group. List of strings.   |  `None` |

**RETURNS**

A JSON-encoded string describing the group.


<a id="passwd_entry"></a>

## passwd_entry

<pre>
load("@rules_img//img:base_images.bzl", "passwd_entry")

passwd_entry(*, <a href="#passwd_entry-username">username</a>, <a href="#passwd_entry-uid">uid</a>, <a href="#passwd_entry-gid">gid</a>, <a href="#passwd_entry-gecos">gecos</a>, <a href="#passwd_entry-home">home</a>, <a href="#passwd_entry-shell">shell</a>)
</pre>

Describes one user for the `users` attribute of `etc_passwd`.

**PARAMETERS**


| Name  | Description | Default Value |
| :------------- | :------------- | :------------- |
| <a id="passwd_entry-username"></a>username |  Login name. String.   |  none |
| <a id="passwd_entry-uid"></a>uid |  Numeric user ID. Integer.   |  none |
| <a id="passwd_entry-gid"></a>gid |  Numeric ID of the user's primary group. Integer.   |  none |
| <a id="passwd_entry-gecos"></a>gecos |  Human-readable description (the GECOS field). String.   |  `None` |
| <a id="passwd_entry-home"></a>home |  Home directory. Defaults to `/`, meaning the user has none.   |  `None` |
| <a id="passwd_entry-shell"></a>shell |  Login shell. Defaults to `/sbin/nologin`, which denies interactive login.   |  `None` |

**RETURNS**

A JSON-encoded string describing the user.


