"""Public API for container image signing configuration.

See the `signing_config` rule for how `img deploy` signs images by delegating to
external signer plugins.
"""

load("//img/private:signing_config.bzl", _signing_config = "signing_config")

signing_config = _signing_config
