"""Unit tests for the prebuilt lockfile schema."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//img/private/prebuilt:lockfile.bzl", "binary_urls", "blob_url", "sha256_of", "token_url")

# Digest of a file, in both spellings a lockfile entry may use.
_SHA256 = "fd7025e15908a4960ecfd647b55b67f06670c09c471bb8f9072fc39735d7f336"
_INTEGRITY = "sha256-/XAl4VkIpJYOz9ZHtVtn8GZwwJxHG7j5By/DlzXX8zY="

# A second digest, to catch a decoder that trims or pads in the wrong place.
_EMPTY_SHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
_EMPTY_INTEGRITY = "sha256-47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU="

def _sha256_of_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(env, _SHA256, sha256_of({"sha256": _SHA256}), "hex digest is taken as is")
    asserts.equals(env, _SHA256, sha256_of({"sha256": "sha256:" + _SHA256}), "a sha256: prefix is accepted")
    asserts.equals(env, _SHA256, sha256_of({"sha256": _SHA256.upper()}), "hex digest is normalized to lowercase")
    asserts.equals(env, _SHA256, sha256_of({"integrity": _INTEGRITY}), "integrity is decoded to hex")
    asserts.equals(env, _EMPTY_SHA256, sha256_of({"integrity": _EMPTY_INTEGRITY}), "integrity is decoded to hex")
    asserts.equals(
        env,
        _SHA256,
        sha256_of({"integrity": _INTEGRITY, "sha256": _SHA256}),
        "agreeing sha256 and integrity resolve to that digest",
    )
    asserts.equals(env, "", sha256_of({"version": "v0.3.19"}), "an entry may state no digest at all")
    asserts.equals(
        env,
        "",
        sha256_of({"integrity": "sha512-Z0X0MDgeAiZWyfCq88ZzHfEfESuwEQGZuvIHhCyeMBKb1eIz5T3+1PGSGRj6Q3F0Xd6xLp5rZ8IEbBS0F+kEAA=="}),
        "an integrity that is not a sha256 cannot name a blob",
    )

    return unittest.end(env)

_sha256_of_test = unittest.make(_sha256_of_test_impl)

def _binary_urls_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(
        env,
        ["https://github.com/bazel-contrib/rules_img/releases/download/v0.3.19/img_linux_amd64"],
        binary_urls({"version": "v0.3.19", "os": "linux", "cpu": "amd64"}),
        "the default template points at the GitHub release asset",
    )
    asserts.equals(
        env,
        ["https://github.com/bazel-contrib/rules_img/releases/download/v0.3.19/img_windows_amd64.exe"],
        binary_urls({"version": "v0.3.19", "os": "windows", "cpu": "amd64"}),
        "windows binaries carry an .exe extension",
    )
    asserts.equals(
        env,
        ["https://mirror.example.com/v0.4.0/img_darwin_arm64", "https://example.com/img"],
        binary_urls({
            "cpu": "arm64",
            "os": "darwin",
            "url_templates": ["https://mirror.example.com/{version}/img_{os}_{cpu}{dot}{extension}", "https://example.com/img"],
            "version": "v0.4.0",
        }),
        "every template is expanded, in order",
    )

    return unittest.end(env)

_binary_urls_test = unittest.make(_binary_urls_test_impl)

def _blob_url_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(
        env,
        "https://ghcr.io/v2/bazel-contrib/rules_img/img/blobs/sha256:" + _SHA256,
        blob_url(registry = "ghcr.io", repository = "bazel-contrib/rules_img/img", sha256 = _SHA256),
    )

    return unittest.end(env)

_blob_url_test = unittest.make(_blob_url_test_impl)

def _token_url_test_impl(ctx):
    env = unittest.begin(ctx)

    asserts.equals(
        env,
        "https://ghcr.io/token?scope=repository:bazel-contrib/rules_img/img:pull&service=ghcr.io",
        token_url(registry = "ghcr.io", repository = "bazel-contrib/rules_img/img"),
        "without a recorded challenge, the endpoint the distribution spec suggests is used",
    )
    asserts.equals(
        env,
        "https://auth.docker.io/token?scope=repository:library/ubuntu:pull&service=registry.docker.io",
        token_url(
            registry = "index.docker.io",
            repository = "library/ubuntu",
            auth_challenge = {"realm": "https://auth.docker.io/token", "service": "registry.docker.io"},
        ),
        "a recorded challenge redirects the token exchange",
    )
    asserts.equals(
        env,
        "https://registry.example.com/v2/auth?scope=repository:image:pull&service=registry.example.com",
        token_url(
            registry = "registry.example.com",
            repository = "image",
            auth_challenge = {"realm": "registry.example.com/v2/auth"},
        ),
        "a scheme-less realm is fetched over https, and service defaults to the registry",
    )

    return unittest.end(env)

_token_url_test = unittest.make(_token_url_test_impl)

def lockfile_test_suite(name):
    """Declare the prebuilt lockfile unit tests.

    Args:
        name: Name for the test suite.
    """
    unittest.suite(
        name,
        _sha256_of_test,
        _binary_urls_test,
        _blob_url_test,
        _token_url_test,
    )
