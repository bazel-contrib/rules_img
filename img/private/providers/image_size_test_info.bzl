"""Private provider connecting the image_size_test aspect and rule."""

DOC = """\
Internal provider produced by the image_size_test aspect: everything the
`img image-size-test` subcommand needs to replay an image's per-layer mtrees,
resolved from a rules_img image (ImageManifestInfo / ImageIndexInfo).
"""

FIELDS = dict(
    spec = "File: the images spec JSON. References each image's per-layer mtree " +
           "and layer metadata files by runfiles rlocation path, in layer order.",
    files = "depset[File]: every per-layer mtree + layer metadata file the spec " +
            "references, to be placed in the test runfiles. Never contains layer blobs.",
)

ImageSizeTestInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
