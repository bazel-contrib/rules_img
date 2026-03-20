#!/bin/bash
set -euo pipefail

# This script is used by the release_files test to run the update tool in test mode
TOOL="{TOOL}"

# MODULE.bazel
MODULE_ROOT=$(dirname "$(realpath "MODULE.bazel")")

# The test runs with no-sandbox, so we should be able to access the source tree
# We need to find the actual source workspace root, not the execroot

# First, check if we have BUILD_WORKSPACE_DIRECTORY set (when run with `bazel run`)
if [[ -n "${BUILD_WORKSPACE_DIRECTORY:-}" ]]; then
    WORKSPACE_ROOT="$BUILD_WORKSPACE_DIRECTORY"
else
    WORKSPACE_ROOT="$MODULE_ROOT"
fi

echo "Running release_files test in workspace: $WORKSPACE_ROOT"

# Verify we found the right location
if [[ ! -f "$WORKSPACE_ROOT/MODULE.bazel" ]]; then
    echo "ERROR: Could not find MODULE.bazel in $WORKSPACE_ROOT"
    echo "Current directory: $(pwd)"
    echo "Environment variables:"
    echo "  BUILD_WORKSPACE_DIRECTORY=${BUILD_WORKSPACE_DIRECTORY:-not set}"
    echo "  TOOL=${TOOL:-not set}"
    echo "  MODULE_ROOT=${MODULE_ROOT:-not set}"
    exit 1
fi

# Run the tool in test mode
exec "$TOOL" -repo_root="$WORKSPACE_ROOT" -test
