# Hacking on rules_img

This guide provides instructions for developers working on rules_img.

### Starlark (Bazel) Files

```bash
# Format all Bazel files (fix mode)
bazel run //util:buildifier.fix

# Check Bazel file formatting (test mode)
bazel test //util:buildifier.check

# Run Gazelle to update BUILD files
bazel run //util:gazelle
```

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
# Run all integration tests
bazel test //e2e:integration_tests

# Run C++ integration tests
cd e2e/cc && bazel test //...

# Run Go integration tests
cd e2e/go && bazel test //...

# Test push functionality
cd e2e/go && bazel run //image:push
```

### Development with Source-Built Tools

When developing rules_img, you can use a source-built version of the Go `img` tool instead of prebuilt binaries. This is useful for testing changes to the tool implementation or applying patches. The `img` tool now also provides the image-pulling functionality used by repository rules, so there is a single tool module (`rules_img_tool`) to override.

#### Setting Up Development Dependencies

Add a dependency on the tool module and register the source-built toolchain in your `MODULE.bazel`:

```starlark
# Add a dependency on the tool module
bazel_dep(name = "rules_img_tool", version = "<version>", dev_dependency = True)

# Register source-built toolchain
register_toolchains(
    "@rules_img_tool//toolchain:all",
    dev_dependency = True,
)
```

#### Module Override Options

You can override the tool module using various Bazel module override mechanisms:

##### Local Development Override

For local development with modifications:

```starlark
# Override with local directory
local_path_override(
    module_name = "rules_img_tool",
    path = "../img_tool",  # Path to your local checkout
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
```

##### Single Version Override with Patches

For applying patches to a specific version:

```starlark
single_version_override(
    module_name = "rules_img_tool",
    patches = ["//patches:img_tool_performance.patch"],
    patch_strip = 2,  # For patches created from rules_img root (strips img_tool/ prefix)
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
