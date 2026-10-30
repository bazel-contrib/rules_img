# Img Toolchain Test Format

This directory contains integration tests for the `img` CLI toolchain using a custom INI-based test format.

## Test File Format

Test files use the `.ini` extension and are structured with sections that define test metadata, file setup, command execution, and assertions.

### Section Types

#### `[test]` - Test Metadata
Defines basic information about the test case:
- `name`: Test case identifier
- `description`: Human-readable test description

```ini
[test]
name = layer_simple
description = Test layer creation with a simple file
```

#### `[file]` - File Setup
Creates test files in the temporary directory. Each `[file]` section creates one file:
- `name`: Filename/path for the created file
- Content follows on subsequent lines until the next section

```ini
[file]
name = hello.txt
Hello World

[file]
name = config.conf
server_port=8080
debug=true
```

#### `[testdata]` - Testdata File References
Copies files from the top-level `testdata/` directory into the test environment:
- `copy = dest_path=src_path_in_testdata`: Copy file from testdata to test directory

```ini
[testdata]
copy = ubuntu_config.json=ubuntu/config
copy = sample_blob=ubuntu/blobs/sha256/602eb6fb314b5fafad376a32ab55194e535e533dec6552f82b70d7ac0e554b1c
```

This allows tests to use real container artifacts (configs, manifests, blobs) from the project's testdata directory.

#### `[command]` - Command Execution
Specifies the `img` subcommand to execute:
- `subcommand`: The img subcommand (e.g., `layer`, `manifest`)
- `args`: Command line arguments (space-separated)
- `expect_exit`: Expected exit code (default: 0)
- `stdin`: Optional stdin input

```ini
[command]
subcommand = layer
args = --add /hello.txt=hello.txt layer.tar.gz
expect_exit = 0
```

#### `[assert]` - Assertions
Define validation checks to run after command execution. Multiple assertions can be specified:

**File Assertions:**
- `file_exists = path`: File must exist
- `file_not_exists = path`: File must not exist
- `file_contains = path, "content"`: File must contain text
- `file_not_contains = path, "content"`: File must not contain text
- `file_size_gt = path, bytes`: File size greater than threshold
- `file_size_lt = path, bytes`: File size less than threshold
- `file_sha256 = path, "hash"`: File SHA256 hash matches
- `file_valid_json = path`: File contains valid JSON
- `file_valid_gzip = path`: File is valid gzip format
- `file_valid_tar = path`: File is valid tar format
- `file_valid_ext4 = path`: File is an ext4 image with a valid superblock (magic, checksum, size)
- `file_valid_cpio = path`: File is a cpio archive in the newc format, made of well-formed records that end with the trailer

**Output Assertions:**
- `stdout_contains = "text"`: Stdout contains text
- `stdout_not_contains = "text"`: Stdout does not contain text
- `stderr_contains = "text"`: Stderr contains text
- `stderr_not_contains = "text"`: Stderr does not contain text
- `stdout_matches_regex = "pattern"`: Stdout matches regex
- `stderr_matches_regex = "pattern"`: Stderr matches regex
- `exit_code = code`: Process exit code equals value

**JSON Assertions:**
- `json_field_exists = path, field`: JSON field exists
- `json_field_equals = path, field, value`: JSON field equals value

```ini
[assert]
file_exists = layer.tar.gz
file_valid_gzip = layer.tar.gz
file_size_gt = layer.tar.gz, 100
json_field_exists = metadata.json, digest
```

## Framework Architecture

The testing framework (`framework.go`) provides:

- **TestFramework**: Main test orchestration with temporary directory management
- **Automatic Discovery**: Loads all `*.ini` files from `testdata/`
- **Command Execution**: Runs `img` binary with specified arguments
- **Rich Assertions**: Comprehensive validation of files, output, and formats
- **Context Support**: Proper cancellation and timeout handling

## Running Tests

Tests run automatically via Bazel:

```bash
bazel test //tests/img_toolchain:img_toolchain_test
```

The framework:
1. Discovers all `.ini` test files in `testdata/`
2. Creates isolated temporary directories for each test
3. Sets up required files as specified in `[file]` sections
4. Executes the specified `img` command
5. Validates all assertions in `[assert]` sections
6. Reports detailed failures with command output

## Example Test Cases

### Basic Tests
- `layer_simple.ini`: Basic layer creation test
- `layer_comprehensive.ini`: Layer creation with metadata and validation
- `manifest_basic.ini`: Basic manifest and config generation
- `layer_help.ini`: Help text validation
- `layer_real_ubuntu_config.ini`: Real-world Ubuntu configuration test

### Testdata Integration Tests
- `manifest_with_testdata.ini`: Manifest operations using real Ubuntu config/manifest
- `layer_with_ubuntu_blobs.ini`: Layer creation with Ubuntu blob data
- `validate_ubuntu_config.ini`: Configuration validation with real Ubuntu config
- `manifest_layer_integration.ini`: Complex integration test combining layers and manifests
- `compress_ubuntu_blob.ini`: Compression testing with real blob data

Each test is self-contained and can be run independently through the framework.

## Testdata Directory Structure

The framework can access files from the top-level `testdata/` directory:

```
testdata/
└── ubuntu/
    ├── config          # Ubuntu container configuration (JSON)
    ├── manifest         # Ubuntu container manifest (JSON)
    ├── root             # Ubuntu root filesystem reference
    └── blobs/
        └── sha256/
            ├── 602eb6f...  # Various Ubuntu layer blobs
            ├── 2abc342...
            └── ...
```

These files provide realistic test data for container operations, allowing tests to validate functionality against real container artifacts.
