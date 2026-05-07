"""Provider definitions"""

load("//img/private/providers:deploy_info.bzl", _DeployInfo = "DeployInfo")
load("//img/private/providers:index_info.bzl", _ImageIndexInfo = "ImageIndexInfo")
load("//img/private/providers:layers_info.bzl", _LayersInfo = "LayersInfo")
load("//img/private/providers:load_config_info.bzl", _LoadConfigInfo = "LoadConfigInfo")
load("//img/private/providers:manifest_info.bzl", _ImageManifestInfo = "ImageManifestInfo")
load("//img/private/providers:pull_info.bzl", _PullInfo = "PullInfo")
load("//img/private/providers:push_config_info.bzl", _PushConfigInfo = "PushConfigInfo")
load("//img/private/providers:single_layer_info.bzl", _SingleLayerInfo = "SingleLayerInfo")

# providers with metadata about image pushes
DeployInfo = _DeployInfo

# providers with push/load configuration
PushConfigInfo = _PushConfigInfo
LoadConfigInfo = _LoadConfigInfo

# providers describing images and their components
SingleLayerInfo = _SingleLayerInfo
LayersInfo = _LayersInfo
ImageManifestInfo = _ImageManifestInfo
ImageIndexInfo = _ImageIndexInfo

# providers with metadata about pulled base images
PullInfo = _PullInfo
