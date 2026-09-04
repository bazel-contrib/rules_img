"""Public API for building container images from Dockerfiles with buildah."""

load("//private:buildah_dockerfile.bzl", _buildah_dockerfile = "buildah_dockerfile")

buildah_dockerfile = _buildah_dockerfile
