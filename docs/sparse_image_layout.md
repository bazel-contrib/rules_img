# Sparse OCI Image Layout

This document describes an internal representation for OCI content-addressable blobs used by rules_img. [It is similar to the OCI Image Layout Specification](https://github.com/opencontainers/image-spec/blob/main/image-layout.md), but has important differences.

## Content

The image layout is as follows:

* `blobs` directory
    * Contains content-addressable blobs
    * A blob has no schema and SHOULD be considered opaque
    * Directory MUST exist and MAY be empty
    * See blobs section
* `sparse-oci-layout` file
    * It MUST exist
    * It MUST be a JSON object
    * It MUST contain an `imageLayoutVersion` field
    * See oci-layout file section
    * It MAY include additional fields
* `root.descriptor.json` file
    * It MUST exist
    * It MUST be an [OCI Content descriptor](https://github.com/opencontainers/image-spec/blob/main/descriptor.md) JSON object.
    * See root.descriptor.json section

Additional files and directories MAY be present and MUST be ignored by implementations that do not understand them.

## Example Layout

```
$ find . -type f
./sparse-oci-layout
./root.descriptor.json
./blobs/sha256/9b15c6e7614b2b17374d1f3e4a0b5e5b1f1e5e8f3c7a6b2d4e8f0a1c3b5d7e9f
./blobs/sha256/a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2
./blobs/sha256/d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5.descriptor.json
```

In this example:

- `9b15c6...` is the manifest blob (referenced by `root.descriptor.json`)
- `a1b2c3...` is the config blob (referenced by the manifest)
- `d4e5f6...` is a layer that is NOT stored as a blob. Instead, only its descriptor metadata is stored as `d4e5f6...descriptor.json`.

## Blobs

The `blobs` directory contains content-addressable blobs, organized as `blobs/<alg>/<encoded>`, where `<alg>` is the hash algorithm (e.g. `sha256`) and `<encoded>` is the digest hex value.

The content of `blobs/<alg>/<encoded>` MUST match the digest `<alg>:<encoded>`.

All non-layer blobs referenced by the image (manifests, image indexes, configs) MUST be present in the `blobs` directory.

Layer blobs MAY be stored in the `blobs` directory (inlining), but implementations MUST NOT assume they are present. A layer descriptor file MUST be stored for each layer regardless of whether the layer blob is inlined. See the [Layer Descriptor Files](#layer-descriptor-files) section.

### Layer Descriptor Files

For each layer referenced by a manifest, a descriptor file MUST be stored at:

```
blobs/<alg>/<encoded>.descriptor.json
```

where `<alg>:<encoded>` is the digest of the layer blob.

The descriptor file MUST be a JSON object conforming to the [OCI Content Descriptor](https://github.com/opencontainers/image-spec/blob/main/descriptor.md) specification. It MUST contain the following fields:

- `mediaType` - the media type of the layer (e.g. `application/vnd.oci.image.layer.v1.tar+gzip`)
- `digest` - the digest of the layer blob, which MUST match the `<alg>:<encoded>` in the file path
- `size` - the size in bytes of the layer blob

The descriptor MAY contain additional fields such as `annotations`.

Example `blobs/sha256/d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5.descriptor.json`:

```json
{
    "mediaType": "application/vnd.oci.image.layer.v1.tar+gzip",
    "digest": "sha256:d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4a5b6c7d8e9f0a1b2c3d4e5",
    "size": 32654
}
```

## sparse-oci-layout File

The `sparse-oci-layout` file serves as a marker for the base of a Sparse OCI Image Layout and provides the version of the image layout in use. Its presence distinguishes this format from a standard OCI Image Layout (which uses an `oci-layout` file).

The file MUST be a JSON object and MUST contain an `imageLayoutVersion` field.

Example:

```json
{
    "imageLayoutVersion": "1.0.0"
}
```

## root.descriptor.json File

The `root.descriptor.json` file is the entry point for the Sparse OCI Image Layout. It replaces the `index.json` file used by the standard OCI Image Layout.

The file MUST be a JSON object conforming to the [OCI Content Descriptor](https://github.com/opencontainers/image-spec/blob/main/descriptor.md) specification. It MUST contain the `mediaType`, `digest`, and `size` fields identifying the root blob.

The root blob is typically an [OCI Image Manifest](https://github.com/opencontainers/image-spec/blob/main/manifest.md) or an [OCI Image Index](https://github.com/opencontainers/image-spec/blob/main/image-index.md). The referenced blob MUST exist in the `blobs` directory.

Example `root.descriptor.json` pointing to a manifest:

```json
{
    "mediaType": "application/vnd.oci.image.manifest.v1+json",
    "digest": "sha256:9b15c6e7614b2b17374d1f3e4a0b5e5b1f1e5e8f3c7a6b2d4e8f0a1c3b5d7e9f",
    "size": 1234
}
```

## Differences from OCI Image Layout

| Aspect | OCI Image Layout | Sparse OCI Image Layout |
|---|---|---|
| Marker file | `oci-layout` | `sparse-oci-layout` |
| Entry point | `index.json` (image index) | `root.descriptor.json` (single descriptor) |
| Layer blobs | Stored in `blobs/` | MAY be inlined in `blobs/`, but typically omitted; `.descriptor.json` metadata files MUST be present |
| Non-layer blobs | Stored in `blobs/` | Stored in `blobs/` (same) |
