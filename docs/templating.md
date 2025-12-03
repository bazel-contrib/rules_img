# Templating and Stamping

The `image_manifest`, `image_index`, `image_push`, and `image_load` rules support Go templates for dynamic configuration. This feature enables flexible image naming and metadata based on build settings and Bazel's workspace status (stamping).

## Overview

Templates allow you to:
- Use different registries/repositories for different environments (dev, staging, prod)
- Include version information from your build system
- Add git commit hashes or timestamps to tags and labels
- Create conditional logic for tag naming
- Inject dynamic metadata into container labels, environment variables, and annotations

## Basic Templating with Build Settings

### 1. Define String Flags

First, create string flags using `bazel_skylib`:

```starlark
load("@bazel_skylib//rules:common_settings.bzl", "string_flag")

string_flag(
    name = "environment",
    build_setting_default = "dev",
)

string_flag(
    name = "region",
    build_setting_default = "us-east-1",
)
```

### 2. Use Templates in image_push

Reference the flags in your `image_push` rule:

```starlark
load("@rules_img//img:push.bzl", "image_push")

image_push(
    name = "push",
    image = ":my_image",

    # Use Go template syntax
    registry = "{{.region}}.registry.example.com",
    repository = "myapp/{{.environment}}",
    tag_list = [
        "latest",
        "{{.environment}}-latest",
    ],

    # Map build settings
    build_settings = {
        "environment": ":environment",
        "region": ":region",
    },
)
```

### 3. Override at Build Time

```bash
# Use default values (dev, us-east-1)
bazel run //:push

# Override for production
bazel run //:push --//:environment=prod --//:region=eu-west-1
```

This would push to:
- Registry: `eu-west-1.registry.example.com`
- Repository: `myapp/prod`
- Tags: `latest`, `prod-latest`

## Stamping with Workspace Status

Stamping allows you to include dynamic build information like git commits, timestamps, and version numbers in your container tags, labels, environment variables, and annotations.

### Requirements for Stamping

**Important**: Stamping requires explicit opt-in at two levels:

1. **Bazel level**: Enable stamping with the `--stamp` flag
   - By default, Bazel disables stamping for build reproducibility and performance
   - You must explicitly add `--stamp` to your build command or `.bazelrc`

2. **Target level**: Enable stamping for specific `image_push`, `image_load`, `image_manifest`, or `image_index` targets
   - Set `stamp = "enabled"` on the target, OR
   - Set `stamp = "auto"` (the default) and use `--@rules_img//img/settings:stamp=enabled`

Both levels must be enabled for stamping to work. If either is disabled, stamp variables will not be available in templates.

### Configure Workspace Status

Create a script that outputs key-value pairs:

```bash
#!/usr/bin/env bash
# File: workspace_status.sh

# Variables prefixed with STABLE_ are included in the cache key.
# If their value changes, the target must be rebuilt.
# Only use for values that rarely update for better performance.
echo "STABLE_CONTAINER_VERSION_TAG v1.2.3"

# Variables without STABLE_ prefix are volatile.
# These variables are not included in the cache key.
# If their values changes, a target may still include
# a stale value from a previous build.
echo "BUILD_TIMESTAMP $(date +%s)"
echo "GIT_COMMIT $(git rev-parse HEAD 2>/dev/null || echo 'unknown')"
echo "GIT_BRANCH $(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo 'unknown')"
echo "GIT_DIRTY $(if git diff --quiet 2>/dev/null; then echo 'clean'; else echo 'dirty'; fi)"
```

Make it executable:
```bash
chmod +x workspace_status.sh
```

Add to `.bazelrc`:
```bash
# Configure workspace status script
build --workspace_status_command=./workspace_status.sh
```

### Stamp Attribute Values

| Value | Behavior |
|-------|----------|
| `"enabled"` | Always use stamp values (if Bazel --stamp is set) |
| `"disabled"` | Never use stamp values |
| `"auto"` | Use the `--@rules_img//img/settings:stamp=enabled` flag (default) |

### Troubleshooting Stamping

**Stamp variables are empty or not replaced:**
1. Check that `--stamp` is set at Bazel level
2. Check that `stamp = "enabled"` or proper flags are set
3. Verify workspace_status_command is executable and in .bazelrc
4. Test your script: `./workspace_status.sh` should output key-value pairs

**Build not reproducible:**
- Use `stamp = "disabled"` for development builds
- Only enable stamping for release builds using configs

**Testing stamp values:**
```bash
# Check what values are available
bazel build --stamp //:push
cat bazel-bin/push_template.json  # Shows template with stamp placeholders
cat bazel-bin/push.json            # Shows expanded values
```

## Advanced Template Features

### Conditional Logic

```starlark
tag_list = [
    # Use version tag if available, otherwise "dev"
    "{{if .STABLE_CONTAINER_VERSION_TAG}}{{.STABLE_CONTAINER_VERSION_TAG}}{{else}}dev{{end}}",

    # Add suffix only in dev environment
    "latest{{if eq .environment \"dev\"}}-dev{{end}}",

    # Complex conditions
    "{{if and .GIT_COMMIT (ne .GIT_BRANCH \"main\")}}{{.GIT_BRANCH}}-{{.GIT_COMMIT}}{{end}}",
]
```

### Combining Build Settings and Stamping

You can use both build settings and stamp values together:

```starlark
image_push(
    name = "push",
    image = ":my_image",

    # Combine region from build setting with stamp info
    registry = "{{.region}}.registry.example.com",
    repository = "{{.organization}}/{{.STABLE_BUILD_USER}}/myapp",
    tag_list = [
        "{{.environment}}-{{.STABLE_CONTAINER_VERSION_TAG}}",
        "{{.environment}}-{{.GIT_COMMIT}}",
    ],

    build_settings = {
        "environment": ":environment",
        "region": ":region",
        "organization": ":organization",
    },
    stamp = "enabled",
)
```

## Accessing Base Image Data

The `image_manifest` rule automatically provides access to the base image's configuration and manifest through template variables. This allows you to reference or extend metadata from the base image.

### Available Base Data

When using a `base` image in `image_manifest`, the following template variables are available:

- **`.base.config`** - The base image's OCI configuration JSON
- **`.base.manifest`** - The base image's OCI manifest JSON

**Important**: All JSON field names are automatically converted to lowercase for case-insensitive access. For example, if the parent config has `"Architecture": "amd64"`, you access it as `.base.config.architecture`.

### Common Use Cases

#### Accessing Architecture and OS

```starlark
image_manifest(
    name = "my_image",
    base = "@alpine",
    layers = [":app_layer"],
    labels = {
        "base.architecture": "{{.base.config.architecture}}",  # e.g., "amd64"
        "base.os": "{{.base.config.os}}",                      # e.g., "linux"
    },
)
```

#### Extending Environment Variables from Parent

The parent image's environment variables are stored as an array of `"KEY=VALUE"` strings in `.base.config.config.env`. Use the `getkv`, `appendkv`, or `prependkv` functions to work with them:

```starlark
image_manifest(
    name = "my_image",
    base = "@distroless_base",
    layers = [":app_layer"],
    env = {
        # Extend PATH from parent by appending a custom directory
        # If the base sets $PATH to "/usr/bin:/bin", this results in "/usr/bin:/bin:/custom/bin"
        "PATH": """{{appendkv .base.config.config.env "PATH" ":/custom/bin"}}""",

        # Or prepend to LD_LIBRARY_PATH
        # If the base sets $LD_LIBRARY_PATH to "/usr/lib", this results in "/opt/lib:/usr/lib"
        "LD_LIBRARY_PATH": """{{prependkv .base.config.config.env "LD_LIBRARY_PATH" "/opt/lib:"}}""",

        # Access other parent env vars
        "PARENT_HOME": """{{getkv .base.config.config.env "HOME"}}""",
    },
)
```

#### Accessing Parent User

```starlark
image_manifest(
    name = "my_image",
    base = "@alpine",
    layers = [":app_layer"],
    labels = {
        # Note: lowercase field names
        "base.user": "{{.base.config.config.user}}",
    },
)
```

#### Accessing Parent Manifest Metadata

```starlark
image_manifest(
    name = "my_image",
    base = "@alpine",
    layers = [":app_layer"],
    annotations = {
        "parent.digest": "{{.base.manifest.config.digest}}",
        "parent.mediatype": "{{.base.manifest.mediatype}}",
    },
)
```

### Parent Data in image_index

The `image_index` rule also provides access to the first manifest's data through `.base.config` and `.base.manifest`:

```starlark
image_index(
    name = "multiarch",
    manifests = [":image_amd64", ":image_arm64"],
    annotations = {
        # Accesses the first manifest's config
        "reference.architecture": "{{.base.config.architecture}}",
    },
)
```

## Template Functions

In addition to standard Go template features, several custom functions are available to help work with OCI image metadata:

### Key-Value Array Functions

OCI image configurations store environment variables and other settings as arrays of `"KEY=VALUE"` strings. These functions help extract and manipulate such values:

#### `getkv`

Extracts a value from a key-value array.

**Syntax**: `getkv <array> <key>`

```starlark
env = {
    # Extract PATH from parent's env array
    "ORIGINAL_PATH": """{{getkv .base.config.config.env "PATH"}}""",
}
```

**Example**: If the parent has `["PATH=/usr/bin", "HOME=/root"]`, then `getkv .base.config.config.env "PATH"` returns `/usr/bin`.

#### `appendkv`

Extracts a value from a key-value array and appends a suffix.

**Syntax**: `appendkv <array> <key> <suffix>`

```starlark
env = {
    # Extend PATH by appending a custom directory
    "PATH": """{{appendkv .base.config.config.env "PATH" ":/custom/bin"}}""",
}
```

**Example**: If parent has `PATH=/usr/bin`, this returns `/usr/bin:/custom/bin`.

If the key doesn't exist in the array, returns just the suffix.

#### `prependkv`

Extracts a value from a key-value array and prepends a prefix.

**Syntax**: `prependkv <array> <key> <prefix>`

```starlark
env = {
    # Extend LD_LIBRARY_PATH by prepending
    "LD_LIBRARY_PATH": """{{prependkv .base.config.config.env "LD_LIBRARY_PATH" "/opt/lib:"}}""",
}
```

**Example**: If parent has `LD_LIBRARY_PATH=/usr/lib`, this returns `/opt/lib:/usr/lib`.

If the key doesn't exist in the array, returns just the prefix.

### String Manipulation Functions

#### `split`

Splits a string by a separator.

**Syntax**: `split <string> <separator>`

```starlark
# Split PATH into components (returns a slice)
{{range split .path ":"}}{{.}}{{end}}
```

#### `join`

Joins a slice of strings with a separator.

**Syntax**: `join <slice> <separator>`

```starlark
# Join slice with colons
{{join .components ":"}}
```

#### `hasprefix`, `hassuffix`

Check if a string has a given prefix or suffix.

**Syntax**: `hasprefix <string> <prefix>` or `hassuffix <string> <suffix>`

```starlark
{{if hasprefix .path "/usr"}}starts with /usr{{end}}
{{if hassuffix .tag "-dev"}}is a dev tag{{end}}
```

#### `trimprefix`, `trimsuffix`

Remove a prefix or suffix from a string.

**Syntax**: `trimprefix <string> <prefix>` or `trimsuffix <string> <suffix>`

```starlark
# Remove /usr prefix
{{trimprefix .path "/usr"}}

# Remove -dev suffix
{{trimsuffix .tag "-dev"}}
```
