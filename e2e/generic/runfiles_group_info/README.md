# RunfilesGroupInfo Example

This directory demonstrates how to use `RunfilesGroupInfo` with `image_layer` to create grouped container image layers.

## Overview

The `RunfilesGroupInfo` provider allows executables to declare their runfiles organized into different groups. This enables `image_layer` to automatically split these runfiles into separate layers, optimizing for:
- **Better caching**: Frequently changing files (app code) are separated from rarely changing files (stdlib, dependencies)
- **Faster builds**: Only affected layers need to be rebuilt when files change
- **Optimized deployments**: Container registries and runtimes can cache stable layers

## Files

- **defs.bzl**: Contains `fake_split_binary` rule that simulates an executable with grouped runfiles
- **BUILD.bazel**: Multiple examples showing different ways to use `RunfilesGroupInfo` with `image_layer`
- **\*.txt**: Sample data files representing different types of dependencies

## Examples

### 1. Auto-grouped layers (default)
```starlark
image_layer(
    name = "grouped_layer_auto",
    srcs = {
        "/app/bin/main": ":split_app",  # Has RunfilesGroupInfo
    },
)
```
Creates multiple layers automatically, one per group found in the executable.

### 2. Explicit layer IDs
```starlark
image_layer(
    name = "grouped_layer_explicit",
    srcs = {
        "/app/bin/main": ":split_app",
    },
    layer_ids = [
        "FOUNDATIONAL_RUNFILES",
        "OTHER_PARTY_RUNFILES",
        "SAME_PARTY_RUNFILES",
    ],
)
```
Creates exactly the specified layers in the given order.

### 3. Custom layer mapping
```starlark
image_layer(
    name = "grouped_layer_custom",
    srcs = {
        "/app/bin/main": ":split_app",
    },
    layer_ids = ["base", "deps", "app"],
    layer_for_group = {
        "FOUNDATIONAL_RUNFILES": "base",
        "OTHER_PARTY_RUNFILES": "deps",
        "SAME_PARTY_RUNFILES": "app",
        "DEBUG_RUNFILES": "app",  # Group debug with app
    },
)
```
Maps multiple groups to custom layer IDs, allowing fine-grained control.

### 4. Filtering groups
```starlark
image_layer(
    name = "grouped_layer_filtered",
    srcs = {
        "/app/bin/main": ":split_app",
    },
    exclude_groups = ["DEBUG_RUNFILES"],
)
```
Excludes certain groups from the final image.

### 5. Merged layers
```starlark
image_layer(
    name = "grouped_layer_merged",
    srcs = {
        "/app/bin/main": ":split_app",
    },
    default_grouping = "merge_all",
)
```
Merges all groups into a single layer (ignores grouping).

## Well-known Groups

The following well-known groups are defined with a specific ordering for optimal layer caching:

1. `FOUNDATIONAL_RUNFILES`: Standard libraries, interpreters, core dependencies
2. `OTHER_PARTY_RUNFILES`: Third-party dependencies
3. `DOCUMENTATION_RUNFILES`: Documentation, help files
4. `DEBUG_RUNFILES`: Debug tools, test utilities
5. `SAME_PARTY_RUNFILES`: Application code (changes most frequently)

Custom groups can also be defined and will be ordered between `DEBUG_RUNFILES` and `SAME_PARTY_RUNFILES`.

## Testing

Run the build tests:
```bash
bazel test //e2e/generic/runfiles_group_info:all_tests
```

Build individual targets:
```bash
bazel build //e2e/generic/runfiles_group_info:grouped_layer_auto
bazel build //e2e/generic/runfiles_group_info:image_custom_grouped
```
