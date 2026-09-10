# Lazy images regression test

Build the image tool, then run the isolated registry integration test:

```sh
bazel --nohome_rc build @rules_img_tool//cmd/img
python3 tests/lazy_images/integration_test.py
```

The test needs Bazel 9.1.1 or later, Python 3, and network access to resolve
Bazel dependencies. It starts a local registry, records requests, and creates
separate output bases, download caches, and repository contents caches. No
Docker daemon is needed. Pass `--keep` to retain the workspace after the test;
the test always stops its Bazel servers.

Coverage includes lockfile fact population/reuse, forced extension reevaluation,
platform selection and incompatibility, standalone manifests, all layer handling
modes, an original-index load tarball with every child preserved, explicit
platform overrides, and multi-platform transitions. Requests for an unselected
platform's manifests, configs, or layers fail the warm-facts checks.

Starlark and Go unit tests cover both downloader implementations' facts behavior:

```sh
bazel --nohome_rc test \
  //img/private/extensions:images_helpers_test \
  //img/private/platforms:selection_test \
  @rules_img_tool//cmd/syncocirefgraph:syncocirefgraph_test
```
