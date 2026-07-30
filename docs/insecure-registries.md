# Insecure (Plain-HTTP) Registries

Local development registries — `k3d`'s built-in registry, `kind`'s registry
container, a bare `docker run registry:2`, MicroK8s' `localhost:32000` — usually
speak **plain HTTP** and have no valid TLS certificate. rules_img addresses
registries over HTTPS by default, so pushing to one of those fails with:

```
Get "https://k3d-myregistry.localhost:12345/v2/": http: server gave HTTP response to HTTPS client
```

Prefixing the registry with `http://` does not help: `registry` is a registry
*host*, not a URL, so `http://…` is rejected as an invalid repository name.
Instead, tell rules_img that the registry is insecure.

## Enabling insecure access

Set the global flag (the equivalent of [crane](https://github.com/google/go-containerregistry/blob/main/cmd/crane/doc/crane.md)'s
`--insecure`):

```bash
bazel run //path/to:push --@rules_img//img/settings:insecure=enabled
```

or, more commonly, in `.bazelrc` for the configuration that deploys locally:

```
build:dev --@rules_img//img/settings:insecure=enabled
```

This does two things for every registry operation:

1. Registries are addressed over `http://` instead of `https://`.
2. TLS certificates are not verified, so a registry that *does* serve HTTPS with
   a self-signed or expired certificate is accepted too.

> **This disables transport security.** Credentials and image data travel in
> plaintext (or to an unverified peer) and are open to interception and
> tampering. Use it for local development registries only, never for a shared or
> production registry. The flag is global — it applies to every registry the
> build talks to, not just the local one.

The setting covers:

- `bazel run` on `image_push`, `image_load`, and `multi_deploy` targets.
- The registry-touching build actions: [push at build time](push-strategies.md#push-at-build-time)
  (`PushImage`) and lazily pulled base-image layers (`DownloadBlob`).

## Registries that are insecure without the flag

The flag is not needed for hosts that are unambiguously local, which are always
reached over HTTP:

- `localhost:PORT`
- any `*.localhost` host (with or without a port)
- the loopback addresses `127.0.0.1` and `::1`
- RFC 1918 addresses (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`)

A k3d registry reached as `k3d-myregistry.localhost:12345` therefore works out of
the box; the same registry reached under a hostname that is not one of the above
(for example an entry you added to `/etc/hosts`, or `host.docker.internal`) needs
the flag.

## The `img` tool

The same switch exists on the `img` tool that the rules invoke, either as a
global flag accepted by any subcommand or as an environment variable:

```bash
img --insecure deploy --request-file request.json
IMG_INSECURE=1 img deploy --request-file request.json
```

Because `image_push` targets forward their arguments, `--insecure` also works
ad hoc without setting the Bazel flag:

```bash
bazel run //path/to:push -- --insecure
```

`IMG_INSECURE` is inherited by `bazel run` deploys, so exporting it in your shell
works as well. Any value other than `0`/`false` enables insecure access.

Base images pulled by the [`pull`](pull.md#pull) repository rule are fetched while
Bazel loads the workspace, before build settings exist, so the Bazel flag does not
apply there. Pass the environment variable into repository fetching instead (with
the default `downloader = "img_tool"`):

```
common --repo_env=IMG_INSECURE=1
```
