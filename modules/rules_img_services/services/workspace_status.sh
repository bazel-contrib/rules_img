#!/bin/sh
# Prints the source the images are built from, as KEY VALUE lines, for the
# provenance attached to them (see //services:provenance). The provenance signer
# plugin runs this at deploy time, from the root of this Bazel workspace, so a
# commit is never a build input.
set -eu

echo "BUILD_SCM_REVISION $(git rev-parse HEAD)"
echo "BUILD_SCM_URL $(git config --get remote.origin.url)"
# The build definition's location in the repository: this module's directory.
echo "BUILD_SCM_PATH $(git rev-parse --show-prefix | sed 's|/$||')"
if git diff --quiet HEAD --; then
  echo "BUILD_SCM_STATUS clean"
else
  echo "BUILD_SCM_STATUS modified"
fi
