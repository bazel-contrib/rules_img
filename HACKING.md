# Hacking on rules_img

This guide provides instructions for developers working on rules_img.

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
# Build all targets
bazel build //...

# Build specific binaries
bazel build //cmd/img      # Main CLI tool
bazel build //cmd/registry # CAS-integrated registry
bazel build //cmd/bes      # BES server
```

### Running Tests

```bash
# Run all tests
bazel test //...

# Run tests with verbose output
bazel test --test_output=all //...
```

### Integration Testing

The main integration tests are in the benchmark directory:

```bash
cd benchmark

# Build example images
bazel build //examples:my_image
bazel build //examples:cc_index

# Test push functionality
bazel run //examples:my_push
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

1. Implement the compressor in `pkg/compress/`
2. Add it to the factory in `pkg/compress/factory.go`
3. Update the compression attribute in `img/private/layer.bzl`
4. Add the option to `img/settings/BUILD.bazel`
5. Update documentation

### Adding a New Push Strategy

1. Implement the pusher in `pkg/push/`
2. Add it to the push command in `cmd/push/push.go`
3. Update the push strategy setting in `img/settings/BUILD.bazel`
4. Document it in `docs/push-strategies.md`

### Debugging

```bash
# Use Bazel's debugging features
bazel build --sandbox_debug //target:name

# Inspect action outputs
bazel aquery //target:name
```

## Repository Structure

```
rules_img/
├── cmd/           # Command-line tools
├── pkg/           # Go libraries
├── img/           # Public Bazel rules
│   └── private/   # Implementation details
├── docs/          # Generated documentation
├── benchmark/     # Performance tests and examples
└── testdata/      # Test fixtures
```

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
# Update go.mod and go.sum
go mod tidy
# Update Bazel's view of Go dependencies
bazel mod tidy
# Update all BUILD files
bazel run //util:gazelle
```

### IDE Not Finding Dependencies

1. Ensure you're using the correct GOPACKAGESDRIVER
2. Run `bazel build //...` to generate all outputs
3. Restart your IDE/language server

## Getting Help

- Check existing issues on GitHub
- Read the [API documentation](docs/)
- Review examples in `benchmark/examples/`
- Ask questions in GitHub Discussions
