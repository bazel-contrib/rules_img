# Registry Package

This package is a vendored copy of
[github.com/google/go-containerregistry/pkg/registry](https://github.com/google/go-containerregistry/tree/main/pkg/registry)
(including its internal dependencies `internal/verify` and `internal/and`).

We vendor this locally because we require changes that are not yet merged upstream:

- **Export `RedirectError`** — allows blob handlers to signal redirects to external storage (S3, upstream registries).
- **Export `ErrNotFound`** — allows blob handlers to signal a blob is missing so the registry can try fallback stores.
- **Add callbacks on PUT and DELETE** — `WithManifestPutCallback`, `WithManifestDeleteCallback`, `WithBlobCreatedCallback`, `WithBlobDeletedCallback` options that notify listeners when manifests/blobs are created or removed.

These changes are tracked in https://github.com/bazel-contrib/rules_img/issues/282.

## Future direction

We intend to extend this package beyond the upstream scope. The upstream
go-containerregistry project treats `pkg/registry` as an internal testing
utility and is not interested in making it production-ready
(see https://github.com/google/go-containerregistry/issues/1166). We use it as
a real registry serving build artifacts and plan to add features such as
improved storage backends, better error handling, and more robust concurrency
support over time.

## Updating from upstream

When updating this code from upstream, apply the above changes on top of the
latest `pkg/registry` from the `main` branch of go-containerregistry.
