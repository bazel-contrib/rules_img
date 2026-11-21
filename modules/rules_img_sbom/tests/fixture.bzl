"""A pulled base image (an index with an amd64 manifest) whose manifest has a CycloneDX SBOM referrer."""

INDEX = "sha256:" + "0" * 64
AMD64 = "sha256:" + "1" * 64
_CONFIG = "sha256:" + "2" * 64
_LAYER = "sha256:" + "3" * 64
SBOM = "sha256:" + "4" * 64
_SBOM_BLOB = "sha256:" + "5" * 64
_EMPTY = "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"

DATA = {
    INDEX: json.encode(dict(
        schemaVersion = 2,
        mediaType = "application/vnd.oci.image.index.v1+json",
        manifests = [dict(
            mediaType = "application/vnd.oci.image.manifest.v1+json",
            digest = AMD64,
            size = 1,
            platform = dict(os = "linux", architecture = "amd64"),
        )],
    )),
    AMD64: json.encode(dict(
        schemaVersion = 2,
        mediaType = "application/vnd.oci.image.manifest.v1+json",
        config = dict(mediaType = "application/vnd.oci.image.config.v1+json", digest = _CONFIG, size = 1),
        layers = [dict(mediaType = "application/vnd.oci.image.layer.v1.tar+gzip", digest = _LAYER, size = 1)],
    )),
    _CONFIG: json.encode(dict(architecture = "amd64", os = "linux", rootfs = dict(type = "layers", diff_ids = ["sha256:" + "6" * 64]))),
    SBOM: json.encode(dict(
        schemaVersion = 2,
        mediaType = "application/vnd.oci.image.manifest.v1+json",
        artifactType = "application/vnd.cyclonedx+json",
        config = dict(mediaType = "application/vnd.oci.empty.v1+json", digest = _EMPTY, size = 2),
        layers = [dict(mediaType = "application/vnd.cyclonedx+json", digest = _SBOM_BLOB, size = 1)],
        subject = dict(mediaType = "application/vnd.oci.image.manifest.v1+json", digest = AMD64, size = 1),
    )),
}

def files(blob, sbom):
    """The files of the fixture: every blob is a placeholder, except the SBOM.

    Args:
        blob: Label of a placeholder file.
        sbom: Label of the SBOM document.

    Returns:
        Dict of digest to label.
    """
    result = {digest: blob for digest in [INDEX, AMD64, _CONFIG, _LAYER, SBOM, _EMPTY]}
    result[_SBOM_BLOB] = sbom
    return result
