# Maintaining Pulled Images

The targets in `@rules_img//img/pull` keep the container images in
`MODULE.bazel` up to date. They read every [`pull`](pull.md#pull) repository
rule call and every [`images.pull` and `images.referrers`](extensions.md) tag
of the root module, in `MODULE.bazel` and every file it pulls in with
`include()`:

| Target                                | Does                                                                                   |
| ------------------------------------- | -------------------------------------------------------------------------------------- |
| `@rules_img//img/pull:update`         | Moves images to newer versions.                                                        |
| `@rules_img//img/pull:pin`            | Adds a digest to images that only have a tag.                                          |
| `@rules_img//img/pull:referrers`      | Finds the referrers of images (signatures, SBOMs, attestations, ...) and pins them.    |
| `@rules_img//img/pull:list`           | Lists the images, without accessing any registry.                                      |

Changes are made with [buildozer](https://github.com/bazelbuild/buildtools/tree/main/buildozer),
which can also just print them. Every target explains itself and its flags with
`-- --help`.

```sh
bazel run @rules_img//img/pull:update                       # what would change
bazel run @rules_img//img/pull:update -- --mode=apply       # change it
bazel run @rules_img//img/pull:update -- --keep-tags        # only new digests for the current tags
bazel run @rules_img//img/pull:pin -- --help
```

## Modes

`update`, `pin` and `referrers` take `--mode`:

| Mode       | Does                                                                              |
| ---------- | --------------------------------------------------------------------------------- |
| `report`   | Prints every image and what would change. The default.                            |
| `commands` | Prints the buildozer commands that make the changes, and nothing else, on stdout. |
| `apply`    | Makes the changes by running buildozer, which must be in `PATH` (or `--buildozer`). |

The commands of `--mode=commands` address calls by file and line, like
`set digest "sha256:..."|//MODULE.bazel:%42`, so run them all at once from the
workspace root, before the files change:

```sh
bazel run @rules_img//img/pull:update -- --mode=commands > update.txt
buildozer -f update.txt
```

`--format=json` prints the report as JSON instead, for scripts.

## Selecting images

Arguments select images by name, repository or digest, with `*` as a wildcard.
Without any, every image is selected:

```sh
bazel run @rules_img//img/pull:update -- 'distroless*' library/nginx
```

## Targets

### `list`

`bazel run @rules_img//img/pull:list` lists the images, where they are declared, their references, and their
referrer settings. It doesn't access any registry.

### `pin`

`bazel run @rules_img//img/pull:pin` resolves the tag of every image without a digest, and sets `digest`. If the
image has `unsafe_allow_tag_without_digest = True`, it is removed, since it
doesn't apply to an image with a digest.

`pin` only reads the files of the root module, without evaluating the images
extension. So it works even while the extension fails, for example because an
`images.pull` has neither `digest` nor `unsafe_allow_tag_without_digest`, which
the extension's error message then points to.

### `referrers`

Lists the referrers of every pinned image, its child manifests if it is an
index, and the manifest of every `images.referrers` tag:

- referrers in the sense of the OCI distribution spec, from the registry's
  referrers API, or the referrers tag schema if the registry has no such API,
- cosign's tag-scheme signatures and attestations (`sha256-<hex>.sig` and
  `sha256-<hex>.att`), checked to name the manifest in their signed payloads,
  like the images extension does,
- the BuildKit attestation manifests (SBOMs, provenance) that an index lists
  for its child manifests.

For each referrer, it prints the artifact type, how it was found, its size,
annotations and layers (with the `predicateType` of attestations), and whether
`referrer_digests` already pins it. `--artifact-type` (repeatable) selects
referrers by artifact type, with `*` as a wildcard; cosign's artifacts have the
types `cosign:sig` and `cosign:att`, BuildKit's attestations the type
`buildkit:attestation`:

```sh
bazel run @rules_img//img/pull:referrers -- --artifact-type=cosign:att
bazel run @rules_img//img/pull:referrers -- --artifact-type='application/vnd.dev.sigstore.*'
```

The referrers not pinned yet are added to `referrer_digests`. See
[Referrers](referrers.md) for what the images extension and `pull` do with them.

### `update`

`bazel run @rules_img//img/pull:update` moves every image to a newer version:

- An image with a tag that contains a version moves to the highest tag of the
  same shape: the same text before and after the version, and as many numbers.
  `3.23` moves to `3.24` (not to `3.24.1` or `3.24-rc1`), `v1.36.3` to
  `v1.37.0`, and `1.27-alpine` to `1.28-alpine`. `--max-bump=minor` keeps the
  first number, `--max-bump=patch` the first two.
- An image with any other tag (like `latest` or `stable`) gets the digest that
  its tag points to now. So does an image with a version tag when there is no
  newer version, or with `--keep-tags` (the same as `--max-bump=none`), which
  never moves a tag: use it for tags like `3.23` that are rebuilt in place.
- An image with only a digest gets the digest that `latest` points to
  (`--latest-tag` picks another tag).
- An image with only a tag (and `unsafe_allow_tag_without_digest`) moves to a
  newer version tag, but doesn't get a digest. Use `pin` for that.

When the digest of an image changes, everything that names the old digest
follows:

- `referrer_digests`: each pinned referrer is replaced by the referrers of the
  same type that apply to the corresponding manifest of the new image (the new
  index, or its child of the same platform).
- `images.referrers` tags of the root module that name the old manifest (or one
  of its children) move to the corresponding new manifest, with their
  `referrer_digests` replaced the same way. A tag stays where it is if another
  image still pulls the old manifest.
- `use_repo` imports of the image repository of an `images.pull`
  (`img_<digest>`).

Re-run your build afterwards: the lockfile is updated with the new images.

## Images of other modules

The images extension also knows the `images.pull` and `images.referrers` tags of
every other module. `list`, `update` and `referrers` read them from the
extension's hub repository, so building them evaluates the extension. They are
listed and checked like those of the root module, but can't be changed. For
referrers, the report shows an `images.referrers` tag that pins them from the
root module. `--root-only` leaves them out.

`pull` repository rule calls of other modules are not visible to the root
module, so they are not listed.

## Registry access

Registries are accessed like the `img` tool does in the images extension:
credential helpers (`$IMG_CREDENTIAL_HELPER`), Docker and Podman config files,
and the `credential_helper`, `config` and `docker_config_path` of the root
module's `images.settings` tag, unless the environment sets them. Sources are
tried in order (`registry`, then `registries`). Tags are resolved with `HEAD`
requests, which Docker Hub doesn't count against its pull rate limit.

## Limitations

- Only attributes written out as literals can be changed. An attribute set from
  a variable (`digest = NGINX_DIGEST`) is reported, and left to edit by hand.
- buildozer formats every file it changes.
- `referrer_artifacts` specs are not looked at. If one names subjects, check
  them after `update`.
