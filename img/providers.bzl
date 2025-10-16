"""Provider definitions"""

load("//img/private/providers:index_info.bzl", _ImageIndexInfo = "ImageIndexInfo")
load("//img/private/providers:layer_group_info.bzl", _LayerGroupInfo = "LayerGroupInfo")
load("//img/private/providers:layer_info.bzl", _LayerInfo = "LayerInfo")
load("//img/private/providers:manifest_info.bzl", _ImageManifestInfo = "ImageManifestInfo")
load("//img/private/providers:pull_info.bzl", _PullInfo = "PullInfo")

# providers describing images and their components
LayerInfo = _LayerInfo
LayerGroupInfo = _LayerGroupInfo
ImageManifestInfo = _ImageManifestInfo
ImageIndexInfo = _ImageIndexInfo

# providers with metadata about pulled base images
PullInfo = _PullInfo
