# Hacking on rules_img

This guide provides instructions for developers working on rules_img.

**Note**: As of v0.2.6, development has been simplified. You no longer need to manually manage lockfiles or build custom binaries. Instead, use Bazel module overrides to work with source-built versions of the Go tools. See [Development with Source-Built Tools](#development-with-source-built-tools) for details.

## Development Environment

### Prerequisites

- [Nix](https://nixos.org/download.html) (recommended for reproducible development environment)
- OR manually install:
  - Bazel
  - Go
  - pre-commit

### Setting up the Development Environment

#### Option 1: Using Nix (Recommended)

```bash
# Enter the development shell
nix develop

# This provides:
# - Bazel with proper configuration
# - Go toolchain
# - pre-commit hooks
# - All other required tools
```

#### Option 2: Manual Setup

If not using Nix, ensure you have all prerequisites installed and set up pre-commit:

```bash
# Install pre-commit hooks
pre-commit install
```

## IDE Setup

### Visual Studio Code

For the best development experience with VSCode, especially when using Nix:

```bash
# The repository includes VSCode settings for Bazel+Nix development
# Create a symlink to use them:
ln -sf .vscode/settings-bazel-nix.json .vscode/settings.json

# If not using Nix, you may need to adjust the GOPACKAGESDRIVER path
# from gopackagesdriver-nix.sh to gopackagesdriver.sh
```

## Code Formatting and Linting

### Pre-commit Hooks

Pre-commit hooks run automatically on `git commit`. They ensure code quality and consistency.

```bash
# Run all pre-commit hooks manually
pre-commit run --all-files

# Run specific hooks
pre-commit run buildifier --all-files
pre-commit run trailing-whitespace --all-files
```

### Starlark (Bazel) Files

```bash
# Format all Bazel files (fix mode)
bazel run //util:buildifier.fix

# Check Bazel file formatting (test mode)
bazel test //util:buildifier.check

# Run Gazelle to update BUILD files
bazel run //util:gazelle
```

### Markdown Files

Generated markdown files in `docs/` are excluded from trailing whitespace and end-of-file checks.

## Building and Testing

### Building Core Components

```bash
# Build all targets in the main rules_img module
bazel build //...

# Build all targets in the rules_img_tool module (Go binaries)
bazel build @rules_img_tool//...

# Build specific Go binaries from the rules_img_tool module
bazel build @rules_img_tool//cmd/img       # Main CLI tool
bazel build @rules_img_tool//cmd/registry  # CAS-integrated registry
bazel build @rules_img_tool//cmd/bes       # BES server
bazel build @rules_img_tool//pkg/...       # Go libraries
```

### Running Tests

```bash
# Run all tests
bazel test //...

# Run tests with verbose output
bazel test --test_output=all //...
```

### Integration Testing

Integration tests are available in the `e2e/` directory:

```bash
# Run C++ integration tests
cd e2e/cc && bazel test //...

# Run Go integration tests
cd e2e/go && bazel test //...

# Test push functionality
cd e2e/go && bazel run //:push
```

### Development with Source-Built Tools

When developing rules_img, you can use source-built versions of the Go tools (`img` and `pull_tool`) instead of prebuilt binaries. This is useful for testing changes to the tool implementations or applying patches.

#### Setting Up Development Dependencies

Add dependencies on the tool modules and register the source-built toolchains in your `MODULE.bazel`:

```starlark
# Add dependencies on the tool modules
bazel_dep(name = "rules_img_tool", version = "<version>", dev_dependency = True)
bazel_dep(name = "rules_img_pull_tool", version = "<version>", dev_dependency = True)

# Register source-built toolchain
register_toolchains(
    "@rules_img_tool//toolchain:all",
    dev_dependency = True,
)
```

#### Module Override Options

You can override the tool modules using various Bazel module override mechanisms:

##### Local Development Override

For local development with modifications:

```starlark
# Override with local directory
local_path_override(
    module_name = "rules_img_tool",
    path = "../img_tool",  # Path to your local checkout
)

local_path_override(
    module_name = "rules_img_pull_tool",
    path = "../pull_tool",
)
```

##### Git Repository Override

For testing against a specific Git commit or branch:

```starlark
# Override with Git repository
git_override(
    module_name = "rules_img_tool",
    remote = "https://github.com/your-fork/rules_img.git",
    strip_prefix = "img_tool",
    commit = "abc123def456",  # Specific commit
    # Or use: branch = "feature-branch"
)

git_override(
    module_name = "rules_img_pull_tool",
    remote = "https://github.com/your-fork/rules_img.git",
    strip_prefix = "pull_tool",
    commit = "abc123def456",
)
```

##### Archive Override

For testing with a custom archive:

```starlark
archive_override(
    module_name = "rules_img_tool",
    urls = ["https://github.com/your-fork/rules_img/archive/abc123def456.tar.gz"],
    integrity = "sha256-...",
    strip_prefix = "rules_img-abc123def456/img_tool",  # Note: includes img_tool subdirectory
)

archive_override(
    module_name = "rules_img_pull_tool",
    urls = ["https://github.com/your-fork/rules_img/archive/abc123def456.tar.gz"],
    integrity = "sha256-...",
    strip_prefix = "rules_img-abc123def456/pull_tool",  # Note: includes pull_tool subdirectory
)
```

##### Single Version Override with Patches

For applying patches to a specific version:

```starlark
single_version_override(
    module_name = "rules_img_tool",
    patches = ["//patches:img_tool_performance.patch"],
    patch_strip = 2,  # For patches created from rules_img root (strips img_tool/ prefix)
)

single_version_override(
    module_name = "rules_img_pull_tool",
    patches = ["//patches:pull_tool_auth_fix.patch"],
    patch_strip = 2,  # For patches created from rules_img root (strips pull_tool/ prefix)
)
```

## Documentation

### Generating API Documentation

```bash
# Generate/update all API docs
bazel run //docs:update

# Check if docs are up to date
bazel test //docs:all
```

### Adding New Rules

When adding new public rules:

1. Create the rule in `img/private/`
2. Export it in the appropriate public `.bzl` file in `img/`
3. Add a `bzl_library` target in `img/BUILD.bazel`
4. Add documentation generation in `docs/BUILD.bazel`
5. Run `bazel run //docs:update`

## Common Development Tasks

### Adding a New Compression Algorithm

1. Implement the compressor in `img_tool/pkg/compress/`
2. Add it to the factory in `img_tool/pkg/compress/factory.go`
3. Update the compression attribute in `img/private/layer.bzl`
4. Add the option to `img/settings/BUILD.bazel`
5. Update documentation

### Adding a New Push Strategy

1. Implement the pusher in `img_tool/pkg/push/`
2. Add it to the push command in `img_tool/cmd/push/push.go`
3. Update the push strategy setting in `img/settings/BUILD.bazel`
4. Document it in `docs/push-strategies.md`

### Debugging

```bash
# Use Bazel's debugging features for rules
bazel build --sandbox_debug //target:name

# Debug Go binaries in the rules_img_tool module
bazel build --sandbox_debug @rules_img_tool//cmd/img

# Inspect action outputs
bazel aquery //target:name

# Run the img tool directly for debugging
bazel run @rules_img_tool//cmd/img -- --help

# Debug with verbose output
bazel run @rules_img_tool//cmd/img -- pull --help
```

## Repository Structure

The repository uses a **dual-module structure**:

```
rules_img/                    # Main module - Bazel rules and extensions
├── img/                      # Public Bazel rules
│   └── private/              # Implementation details
├── docs/                     # Generated documentation
├── img_tool/                 # rules_img_tool module - Go code
│   ├── cmd/                  # Command-line tools
│   ├── pkg/                  # Go libraries
│   └── MODULE.bazel          # Separate Bazel module
└── e2e/                      # Integration tests and examples
```

### Module Breakdown:

- **`rules_img`** (root): Contains Bazel rules, extensions, and public API
- **`rules_img_tool`** (src/): Contains Go binaries and libraries used by the rules

This separation allows for better dependency management and enables the Go tools to be distributed independently.

## Troubleshooting

### Bazel Cache Issues

```bash
# Clear Bazel cache
bazel clean --expunge

# Clear specific outputs
bazel clean
```

### Go Module Issues

```bash
# Update go.mod and go.sum in the rules_img_tool module
(cd src && go mod tidy)

# Update Bazel's view of Go dependencies
bazel mod tidy

# Update all BUILD files
bazel run //util:gazelle
```

### IDE Not Finding Dependencies

1. Ensure you're using the correct GOPACKAGESDRIVER
2. Run `bazel build //...` and `bazel build @rules_img_tool//...` to generate all outputs
3. Make sure your IDE is pointed at the `src/` directory for Go development
4. Restart your IDE/language server

## Getting Help

- Check existing issues on GitHub
- Read the [API documentation](docs/)
- Review examples in `benchmark/examples/`
- Ask questions in GitHub Discussions
