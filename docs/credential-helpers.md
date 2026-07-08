# Bazel Credential Helpers

`rules_img` can use a [Bazel credential helper](https://github.com/bazelbuild/proposals/blob/main/designs/2022-06-07-bazel-credential-helpers.md)
to authenticate registry and remote-execution requests. A credential helper is
any executable implementing that protocol: it's invoked as `<helper> get`,
receives a JSON request on stdin, and writes a JSON response to stdout, e.g.:

Request:
```json
{"uri": "https://gcr.io"}
```

Response:
```json
{
  "headers": {
    "Authorization": ["Bearer ya29.redacted"]
  },
  "expires": "2026-07-08T12:00:00Z"
}
```

This doc explains exactly where and how `rules_img` invokes such a helper.

## During image pulling

- Through Bazel's own downloader, when a `pull()` repository rule (or the
  `images.pull()` module extension) is configured with `downloader = "bazel"`
  (used for base images fetched from an OCI registry). This requires Bazel's
  own `--credential_helper` flag to be set accordingly — `rules_img`'s own
  `credential_helper` setting is not consulted on this path.
- Through the `img` tool, when it downloads from an OCI registry as part of a
  repository rule or the module extension. This is configured via
  `--@rules_img//img/settings:credential_helper` (or the `credential_helper`
  attribute on the individual `pull()`), falling back to `$IMG_CREDENTIAL_HELPER`.
- **Not currently supported** for lazy layer downloads (`layer_handling =
  "lazy"`): those happen inside a build action, and we haven't found a way to
  make a credential helper available there yet.

## During image loading and pushing

### Authenticating to the remote execution system

The primary use of `credential_helper` during `img load` and the `lazy` /
`cas_registry` push strategies is authenticating gRPC calls to Bazel's remote
cache / remote execution API (REAPI). For each call, `rules_img` derives a URI
from the gRPC target host and the full method name, and asks the credential
helper for headers to attach, e.g.:

```
https://{hostname}/build.bazel.remote.execution.v2.ContentAddressableStorage
https://{hostname}/google.bytestream.ByteStream
```

Whatever headers the helper returns in its response are copied verbatim onto
the outgoing gRPC metadata. Unlike registry authentication below, every header
name is passed through unchanged, not just `Authorization`.

### Authenticating to a container registry

For pushing (and for pulling through the `img` tool, as described above),
`rules_img` also uses `credential_helper` as a container registry keychain via
[go-containerregistry](https://github.com/google/go-containerregistry). Here
the helper is queried with the bare registry host as the URI, e.g.:

```json
{"uri": "registry.example.com"}
```

The response's `Authorization` header is interpreted as follows:

- `Basic <base64(user:pass)>` — decoded and used as the registry
  username/password.
- `Bearer <token>` — treated as a ready-to-use **registry access token** and
  sent through as-is on subsequent registry requests.

The `Bearer` case is worth calling out explicitly: a Bazel credential helper
hands back headers meant to be attached directly to an HTTP request.
`rules_img` must not treat that value as an OAuth2 refresh/identity token and
try to exchange it at the registry's token endpoint — it is already a usable
access token, and exchanging it would either fail or send it to the wrong
endpoint.
