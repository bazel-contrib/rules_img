#!/usr/bin/env bash

# This script is invoked by release.yaml (the main, co-versioned rules_img +
# img_tool release) and by release_signer_plugins.yaml (one independently
# versioned signer plugin), both through the same reusable release workflow. The
# tag decides which release to prepare.

set -euo pipefail

# Argument provided by reusable workflow caller, see
# https://github.com/bazel-contrib/.github/blob/d197a6427c5435ac22e56e33340dff912bc9334e/.github/workflows/release_ruleset.yaml#L72
TAG=$1

# Prepare a signer plugin release from tag `rules_img_signer_<basename>-v<version>`:
# the prebuilt per-platform plugin binaries and the versioned BCR source archive
# (with the populated prebuilt_lockfile.json), all produced hermetically by
# Bazel. We only extract the resulting tarball into dist/ with plain `tar -xf`
# (portable across platforms; no awk/sed/tar --transform). Building from the repo
# root is what lets the cosign plugin compile without rewriting its MODULE.bazel.
release_prep_signer_plugin() {
  local basename="$1"
  local target="//img/private/release:signer_${basename}_dist_tar"
  local tarball

  echo "Building signer plugin release bundle ${target}..." 1>&2
  bazel build "${target}" 1>&2
  tarball="$(bazel cquery --output=files "${target}")"

  echo "Extracting tarball to dist directory..." 1>&2
  mkdir -p dist
  tar -xf "${tarball}" -C dist

  # Both the BCR entry (.bcr/modules/*/source.template.json) and the provenance
  # attestation the registry verifies refer to the archive as <tag>.tar.gz.
  local archive="${TAG}.tar.gz"
  if [[ ! -f "dist/${archive}" ]]; then
    echo "expected release archive dist/${archive} not found;" 1>&2
    echo "does version() in //img/private/release match the released tag?" 1>&2
    exit 1
  fi

  # The per-platform binaries the packaged prebuilt_lockfile.json points at.
  local binaries=(dist/"${basename}"_*)
  if [[ ! -e "${binaries[0]}" ]]; then
    echo "expected prebuilt ${basename} binaries in dist/, found:" 1>&2
    ls dist 1>&2
    exit 1
  fi

  # Release notes, on stdout.
  cat <<EOF
## Using Bzlmod

Add the following to your \`MODULE.bazel\` file:

\`\`\`starlark
bazel_dep(name = "rules_img_signer_${basename}", version = "${TAG##*-v}")
\`\`\`

\`@rules_img_signer_${basename}\` resolves to the prebuilt plugin binary published
with this release. See the
[\`rules_img_signer_${basename}\` README](https://github.com/bazel-contrib/rules_img/blob/${TAG}/modules/rules_img_signer_${basename}/README.md)
for \`signing_config\` recipes, and
[image signing](https://github.com/bazel-contrib/rules_img/blob/${TAG}/docs/image-signing.md)
for the full signing guide.
EOF
}

rm -rf dist

case "${TAG}" in
  rules_img_signer_cosign-v*)
    release_prep_signer_plugin cosign
    exit 0
    ;;
  rules_img_signer_notation-v*)
    release_prep_signer_plugin notation
    exit 0
    ;;
esac

ARCHIVE="rules_img-$TAG.tar.gz"

# Build the distribution tarball
echo "Building distribution tarball..." 1>&2
bazel build //img/private/release:dist_tar 1>&2

# Get the output file location using bazel cquery
TARBALL=$(bazel cquery --output=files //img/private/release:dist_tar)

# Create dist directory if it doesn't exist
mkdir -p dist

# Extract the tarball to the dist directory
echo "Extracting tarball to dist directory..." 1>&2
tar -xvf "$TARBALL" -C dist 1>&2

echo "Packaging Starlark docs..." 1>&2
# Add generated API docs to the release, see https://github.com/bazelbuild/bazel-central-registry/issues/5593
docs="$(mktemp -d)"; targets="$(mktemp)"
bazel --output_base="$docs" query --output=label --output_file="$targets" 'kind("starlark_doc_extract rule", //...)'
bazel --output_base="$docs" build --target_pattern_file="$targets" --remote_download_regex='.*doc_extract\.binaryproto'
tar --create --auto-compress \
    --directory "$(bazel --output_base="$docs" info bazel-bin)" \
    --file "dist/${ARCHIVE%.tar.gz}.docs.tar.gz" .

echo "Release preparation completed. Distribution files are in the 'dist' directory." 1>&2

# Generate release notes using Bazel
echo "Generating release notes using Bazel..." 1>&2

# Build release notes using Bazel
bazel build --output_groups=release_notes //img/private/release:versioned_src_tar 1>&2

# Get the release notes file location using bazel cquery
RELEASE_NOTES_FILE=$(bazel cquery --output=files //img/private/release:versioned_src_tar --output_groups=release_notes)

# Output release notes to stdout
cat "$RELEASE_NOTES_FILE"
