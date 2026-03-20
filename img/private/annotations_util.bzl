"""Utilities to compute annotations"""

BASE_IMAGE_NAME_KEY = "org.opencontainers.image.base.name"

def extract_annotations_from_pull_info(pull_info):
    return {
        BASE_IMAGE_NAME_KEY: _normalized_name_from_pull_info(pull_info),
    }

def _normalized_name_from_pull_info(pull_info):
    tag_extension = ""
    if len(pull_info.tag) > 0:
        tag_extension = ":" + pull_info.tag

    registry = ""
    if len(pull_info.registries) == 0:
        registry = "docker.io"
    else:
        registry = pull_info.registries[0]

    return "{registry}/{repository}{tag_extension}".format(
        registry = registry,
        repository = pull_info.repository,
        tag_extension = tag_extension,
    )
