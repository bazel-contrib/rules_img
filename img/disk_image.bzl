"""Public API for disk images with a GPT or MBR partition table."""

load("//img/private:disk_image.bzl", _disk_image = "disk_image", _disk_partition = "disk_partition")

disk_image = _disk_image
disk_partition = _disk_partition
