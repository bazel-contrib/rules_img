<!-- Generated with Stardoc: http://skydoc.bazel.build -->

Public API for writing the bytes of disk images and layers, like dd.

<a id="image_dd"></a>

## image_dd

<pre>
load("@rules_img//img:dd.bzl", "image_dd")

image_dd(<a href="#image_dd-name">name</a>, <a href="#image_dd-compact">compact</a>, <a href="#image_dd-deploy_tool">deploy_tool</a>, <a href="#image_dd-image">image</a>, <a href="#image_dd-strategy">strategy</a>, <a href="#image_dd-tool_cfg">tool_cfg</a>)
</pre>

(Experimental) Writes the bytes of a disk image or a layer, like `dd`.

The target is executable: `bazel run //:write_disk -- of=/dev/sdX bs=4M status=progress`
writes (or reconstructs) the bytes of the disk image or the layer to a block device,
a file or, without `of=`, to stdout. A layer is what `image_layer` produces,
for example a filesystem image (`format = "ext4"`, `"fat32"`, `"erofs"`) or an
initramfs (`format = "cpio"`) that goes onto a partition or a boot medium. It takes the operands of `dd(1)` as `name=value` or as
`--name=value`:

| Operand | |
|---------|-|
| `of=FILE` | where to write: a file, a block device, or `-` for stdout (default) |
| `bs=BYTES` | block size for writing and for detecting holes (default 1MiB) |
| `skip=N`, `count=N` | skip N input blocks, copy only N input blocks (`iflag=skip_bytes,count_bytes`: in bytes) |
| `seek=N` | skip N output blocks, leaving them as they are (`oflag=seek_bytes`: in bytes) |
| `status=none\|noxfer\|progress` | what to print to stderr (default: a summary at the end) |
| `conv=sparse` | seek over holes and blocks of zeros instead of writing them |
| `conv=nosparse` | write zeros |
| `conv=notrunc`, `conv=fsync` | don't truncate the output file; synchronize before finishing |

Holes (free space of the disk, and blocks of zeros) are not written to an output
file that is created or truncated, which leaves the file sparse. For an existing
device, or with `conv=notrunc`, the zeros are written, unless `conv=sparse` promises
that the output already reads as zeros (a new or erased device). Writing to a
device is destructive: the target does not ask.

If `image` is a `disk_image` built as an (experimental) extent stream
(`extent_stream = "enabled"`), or a layer built as a compact stream
(`experimental_compact_layers`), the target reconstructs the bytes from it. With
`strategy = "lazy"` (or `--@rules_img//img/settings:load_strategy=lazy`), only the extent
stream or compact stream is part of the runfiles: the streams and blobs it
references are fetched by digest when they are needed, from the Bazel disk cache
(`IMG_DISK_CACHE`) or the remote cache (`IMG_REAPI_ENDPOINT`, see
`--@rules_img//img/settings:remote_cache`), and only the parts that `skip` and `count`
select are read. With `eager`, the content-addressed directories are part of the
runfiles and are downloaded by Bazel before the target runs (a layer with a layer
file uses the file instead).

A disk image that is a raw image, and a layer that is only a file, are lazy the same
way: the runfiles only have a small description of the file (its digest and size,
which a layer has already), and the bytes are fetched by digest, from the same
sources, when the target runs. With `eager`, the file is part of the runfiles.

With `compact = "enabled"`, `image` is built as compact streams and an extent stream all
the way, whatever the command line says (see `compact`).

Operands that should always be passed can be set with the `args` attribute
(`args = ["bs=4M", "status=progress"]`); the ones on the command line come after them.

The image is written by the `img dd` command.

**ATTRIBUTES**


| Name  | Description | Type | Mandatory | Default |
| :------------- | :------------- | :------------- | :------------- | :------------- |
| <a id="image_dd-name"></a>name |  A unique name for this target.   | <a href="https://bazel.build/concepts/labels#target-names">Name</a> | required |  |
| <a id="image_dd-compact"></a>compact |  (Experimental) Whether to build `image` as compact streams and extent streams all the way.<br><br>- **`auto`** (default): `image` is built with the settings of the command line. - **`enabled`**: layers are built as compact streams (`experimental_compact_layers`, without materializing the layer blob) and a `disk_image` as an extent stream (`disk_image_extent_stream`). The image then only references the files it is made of, which are fetched by digest when the target runs (see `strategy`), instead of being built in full. - **`disabled`**: layers and disk images are built in full.<br><br>This changes the configuration of `image` and everything it depends on, so it is built separately from the same targets built with another value.   | String | optional |  `"auto"`  |
| <a id="image_dd-deploy_tool"></a>deploy_tool |  Optional label of a deploy tool target providing `DeployToolInfo` (created with `img_deploy_tool` from `@rules_img//img:deploy_tool.bzl`). When set, overrides `tool_cfg`.   | <a href="https://bazel.build/concepts/labels">Label</a> | optional |  `None`  |
| <a id="image_dd-image"></a>image |  The `disk_image` or the layer (a target of `image_layer` or `layer_from_tar` that produces one layer) to write.   | <a href="https://bazel.build/concepts/labels">Label</a> | required |  |
| <a id="image_dd-strategy"></a>strategy |  How the bytes are made available.<br><br>- **`auto`** (default): uses the global `--@rules_img//img/settings:load_strategy`. - **`eager`**: the file, or the files that a stream references, are part of the runfiles. - **`lazy`**: only a stream or a description of the file (digest and size) is. The rest is fetched by digest when it is needed.   | String | optional |  `"auto"`  |
| <a id="image_dd-tool_cfg"></a>tool_cfg |  **Experimental**: Configuration of the `img` executable. By default it matches the host platform; `"target"` makes it match the target platform.   | String | optional |  `"host"`  |


