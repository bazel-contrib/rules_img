"""Private provider connecting the image_structure_test aspect and rule."""

DOC = """\
Internal provider produced by the image_structure_test aspect: everything the
`img image-structure-test` subcommand needs to validate an image, resolved from
either a rules_img image (ImageManifestInfo / ImageIndexInfo) or an OCI image
layout directory (rules_oci).
"""

FIELDS = dict(
    spec = "File: the images spec JSON (the \"what to check\" instruction file). " +
           "References each image's config JSON and mtree by runfiles rlocation path, " +
           "or an OCI layout metadata tree to be read at run time.",
    files = "depset[File]: every config + mtree file (or the OCI layout metadata tree) " +
            "the spec references, to be placed in the test runfiles. Never contains layer blobs.",
)

ImageStructureTestInfo = provider(
    doc = DOC,
    fields = FIELDS,
)
