#!/usr/bin/env bash
# Builds a rules_img signer plugin module for release: compiles the per-platform
# binaries, computes their SRI digests into prebuilt_lockfile.json, and packages
# the module source (with the populated lockfile) as a BCR source archive.
#
# Usage: release_signer_plugin.sh <module_dir> <basename> <tag>
#   module_dir  path to the module (e.g. modules/rules_img_signer_cosign)
#   basename    release asset basename (e.g. cosign or notation)
#   tag         release tag used in download URLs (e.g. rules_img_signer_cosign-v0.0.1)
#
# Outputs into dist/:
#   <basename>_<os>_<cpu>[.exe]          prebuilt binaries (release assets)
#   rules_img_signer_<basename>-<version>.tar.gz  source archive for the BCR
#
# The module's committed MODULE.bazel does not carry the (root-only) gazelle
# proto overrides required by sigstore; this script injects them for the build
# only and packages the clean, committed MODULE.bazel in the source archive.
set -euo pipefail

MODULE_DIR="$1"
BASENAME="$2"
TAG="$3"

PLATFORMS=(
  "linux amd64"
  "linux arm64"
  "darwin amd64"
  "darwin arm64"
  "windows amd64"
  "windows arm64"
)

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="${REPO_ROOT}/dist"
mkdir -p "${DIST}"

# Proto overrides needed to build the sigstore-based cosign plugin from source.
# gazelle_override is root-only, so it can only live in the module's MODULE.bazel
# while the module is the root (its own release build), not in the published
# source. We inject it here and restore the clean file afterwards.
PROTO_OVERRIDE=""
if [[ "${BASENAME}" == "cosign" ]]; then
  PROTO_OVERRIDE=$'go_deps.gazelle_override(\n    directives = ["gazelle:go_generate_proto false"],\n    path = "github.com/sigstore/rekor-tiles/v2",\n)\ngo_deps.gazelle_override(\n    directives = ["gazelle:go_generate_proto false"],\n    path = "github.com/google/certificate-transparency-go",\n)'
fi

cd "${MODULE_DIR}"
VERSION="$(sed -n 's/^\s*version = "\(.*\)",$/\1/p' MODULE.bazel | head -1)"

cp MODULE.bazel MODULE.bazel.release-bak
trap 'mv -f MODULE.bazel.release-bak MODULE.bazel 2>/dev/null || true' EXIT
if [[ -n "${PROTO_OVERRIDE}" ]]; then
  # Insert the overrides right after the go_deps.from_file line.
  awk -v ins="${PROTO_OVERRIDE}" '
    { print }
    /^go_deps.from_file\(/ { print ins }
  ' MODULE.bazel.release-bak > MODULE.bazel
fi

# Build each platform binary and record its SRI digest.
lockfile="["
first=1
for entry in "${PLATFORMS[@]}"; do
  read -r os cpu <<<"${entry}"
  ext=""
  [[ "${os}" == "windows" ]] && ext=".exe"
  target="//cmd/${BASENAME}:${BASENAME}_${os}_${cpu}"
  bazel build "${target}"
  out="$(bazel cquery --output=files "${target}" 2>/dev/null | head -1)"
  asset="${DIST}/${BASENAME}_${os}_${cpu}${ext}"
  cp -f "${out}" "${asset}"
  sri="sha256-$(openssl dgst -sha256 -binary "${asset}" | openssl base64 -A)"
  [[ ${first} -eq 0 ]] && lockfile+=","
  first=0
  lockfile+=$(printf '{"version":"%s","integrity":"%s","os":"%s","cpu":"%s"}' "${TAG}" "${sri}" "${os}" "${cpu}")
done
lockfile+="]"

# Restore the clean MODULE.bazel before packaging the source archive.
mv -f MODULE.bazel.release-bak MODULE.bazel
trap - EXIT

# Package the module source with the populated lockfile.
echo "${lockfile}" > prebuilt_lockfile.json
archive="${DIST}/rules_img_signer_${BASENAME}-${VERSION}.tar.gz"
tar --exclude bazel-\* --exclude '*.release-bak' -czf "${archive}" \
  --transform "s,^\.,rules_img_signer_${BASENAME}," .
# Reset the lockfile so the working tree stays clean.
printf '[]\n' > prebuilt_lockfile.json

echo "Wrote ${archive} and dist/${BASENAME}_* binaries (version ${VERSION}, tag ${TAG})."
