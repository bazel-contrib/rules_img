"""Provider definitions"""

load("//img/private/providers:deploy_info.bzl", _DeployInfo = "DeployInfo")
load("//img/private/providers:index_info.bzl", _ImageIndexInfo = "ImageIndexInfo")
load("//img/private/providers:layer_info.bzl", _LayerInfo = "LayerInfo")
load("//img/private/providers:manifest_info.bzl", _ImageManifestInfo = "ImageManifestInfo")
load("//img/private/providers:pull_info.bzl", _PullInfo = "PullInfo")

# providers with metadata about image pushes
DeployInfo = _DeployInfo

# providers describing images and their components
LayerInfo = _LayerInfo
ImageManifestInfo = _ImageManifestInfo
ImageIndexInfo = _ImageIndexInfo

# providers with metadata about pulled base images
PullInfo = _PullInfo
