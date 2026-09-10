#!/usr/bin/env python3
"""Exercise actual Bazel repository laziness against a request-counting registry.

Run manually: python3 tests/lazy_images/integration_test.py
Requires Bazel, network access for Bazel dependencies, and a locally built img tool:
  bazel --nohome_rc build @rules_img_tool//cmd/img
The test workspace, registry, and output bases are isolated in a temporary directory.
"""

import argparse
import hashlib
import http.server
import io
import json
import os
import platform
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import threading


def encoded(value):
    return json.dumps(value, separators=(",", ":")).encode()


def digest(data):
    return "sha256:" + hashlib.sha256(data).hexdigest()


def fixtures():
    blobs = {}
    manifests = {}
    layers = {}
    configs = {}
    children = []
    for arch in ("amd64", "arm64", "unknown"):
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w") as archive:
            data = arch.encode()
            entry = tarfile.TarInfo("architecture")
            entry.size = len(data)
            archive.addfile(entry, io.BytesIO(data))
        layer = buf.getvalue()
        ld = digest(layer)
        blobs[ld] = layer
        layers[arch] = ld
        config = encoded({"architecture": arch, "os": "unknown" if arch == "unknown" else "linux", "rootfs": {"type": "layers", "diff_ids": [ld]}, "config": {}})
        cd = digest(config)
        blobs[cd] = config
        configs[arch] = cd
        manifest = encoded({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json", "config": {"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cd, "size": len(config)}, "layers": [{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": ld, "size": len(layer)}]})
        md = digest(manifest)
        manifests[md] = manifest
        children.append({"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": md, "size": len(manifest), "platform": {"os": "unknown" if arch == "unknown" else "linux", "architecture": arch}})
    index = encoded({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": children})
    root = digest(index)
    manifests[root] = index
    return root, children, manifests, blobs, configs, layers


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bazel", default="bazel")
    parser.add_argument("--keep", action="store_true")
    parser.add_argument("--img", type=Path, help="Already built host img executable")
    args = parser.parse_args()
    repo = Path(__file__).resolve().parents[2]
    if args.img:
        img = args.img.resolve()
    else:
        output = subprocess.check_output(
            [args.bazel, "--nohome_rc", "cquery", "@rules_img_tool//cmd/img",
             "--output=files", "--noshow_progress"], cwd=repo, text=True,
        ).strip()
        img = (repo / output).resolve()
    if not img.is_file():
        raise SystemExit("Build @rules_img_tool//cmd/img first")
    root, children, manifests, blobs, configs, layers = fixtures()
    requests = []

    class Registry(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            requests.append(self.path)
            if self.path == "/v2/":
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b"{}")
                return
            kind, key = self.path.rsplit("/", 2)[-2:]
            data = (manifests if kind == "manifests" else blobs).get(key)
            if data is None:
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Type", json.loads(data)["mediaType"] if kind == "manifests" else "application/octet-stream")
            self.send_header("Docker-Content-Digest", key)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, *_):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Registry)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    work = Path(tempfile.mkdtemp(prefix="rules-img-lazy-"))
    print(f"Workspace: {work}", flush=True)
    try:
        (work / "MODULE.bazel").write_text(f'''module(name = "lazy_test")
bazel_dep(name = "rules_img", version = "0.3.21")
local_path_override(module_name = "rules_img", path = {str(repo)!r})
bazel_dep(name = "rules_img_tool", version = "0.3.21")
local_path_override(module_name = "rules_img_tool", path = {str(repo / 'img_tool')!r})
bazel_dep(name = "platforms", version = "1.1.0")
bazel_dep(name = "hermetic_launcher", version = "0.0.10")
register_toolchains("//:tools")
images = use_extension("@rules_img//img:extensions.bzl", "images")
images.pull(name = "fixture", registry = "127.0.0.1:{server.server_port}", repository = "test", digest = {root!r}, layer_handling = "eager")
images.pull(name = "standalone", registry = "127.0.0.1:{server.server_port}", repository = "test", digest = {children[0]["digest"]!r})
images.settings(hub_repo = "enabled")
use_repo(images, "rules_img_images.bzl")
''')
        (work / "BUILD.bazel").write_text('''load("@rules_img_images.bzl", "image", "original_image")
load("@rules_img//img:image.bzl", "image_manifest", "image_index")
alias(name = "selected", actual = image("fixture"))
alias(name = "original", actual = original_image("fixture"))
alias(name = "standalone", actual = image("standalone"))
alias(name = "standalone_original", actual = original_image("standalone"))
platform(name = "amd64", constraint_values = ["@platforms//os:linux", "@platforms//cpu:x86_64"])
platform(name = "arm64", constraint_values = ["@platforms//os:linux", "@platforms//cpu:aarch64"])
platform(name = "windows", constraint_values = ["@platforms//os:windows", "@platforms//cpu:x86_64"])
image_manifest(name = "app", base = image("fixture"))
image_manifest(name = "arm_app", base = image("fixture"), platform = ":arm64")
image_index(name = "both", manifests = [":app"], platforms = [":amd64", ":arm64"])
''')
        with (work / "BUILD.bazel").open("a") as out:
            out.write('load("@rules_img//img:image_toolchain.bzl", "image_toolchain")\n')
            out.write('image_toolchain(name = "local_tool", tool_exe = "@local_img//:img", exec_os = %r)\n' % ("darwin" if platform.system() == "Darwin" else "linux"))
            out.write('toolchain(name = "tools", toolchain = ":local_tool", toolchain_type = "@rules_img//img:toolchain_type")\n')
            out.write('load("@rules_img//img:load.bzl", "image_load")\n')
            out.write('load("@rules_img//img:deploy_tool.bzl", "img_deploy_tool")\n')
            out.write('img_deploy_tool(name = "deploy_tool", img_deploy_exe = "@local_img//:img", launcher_template = "@hermetic_launcher//launcher/template:prebuilt_for_host")\n')
            out.write('image_load(name = "load_original", image = original_image("fixture"), tag = "test:latest", deploy_tool = ":deploy_tool")\n')
        # Use the already built tool for both repository execution and actions.
        (work / "tool").mkdir()
        (work / "tool/MODULE.bazel").write_text('module(name = "local_img")\n')
        (work / "tool/BUILD.bazel").write_text('exports_files(["img"])\n')
        shutil.copyfile(img, work / "tool/img")
        (work / "tool/img").chmod(0o755)
        with (work / "MODULE.bazel").open("a") as out:
            out.write('bazel_dep(name = "local_img", version = "0.0.0")\nlocal_path_override(module_name = "local_img", path = "tool")\n')
        with (work / "MODULE.bazel").open("a") as out:
            out.write('prebuilt = use_extension("@rules_img//img:extensions.bzl", "prebuilt_img_tool")\nprebuilt.host_tool(binary = "@local_img//:img")\n')
        env = dict(os.environ, IMG_INSECURE="1", IMG_REGISTRY_RETRY_MAX_ATTEMPTS="1")

        def run(label, platform="amd64", fresh=False, expect_success=True, extra=()):
            suffix = str(run.number) if fresh else "warm"
            run.number += 1
            command = [args.bazel, "--nohome_rc", f"--output_base={work / ('output-' + suffix)}", "build", label, f"--platforms=//:{platform}", "--noshow_progress", "--color=no", "--curses=no", "--lockfile_mode=update", f"--repository_cache={work / ('cache-' + suffix)}", f"--repo_contents_cache={work / ('repos-' + suffix)}"]
            command.extend(extra)
            run.last_output = work / ('output-' + suffix)
            result = subprocess.run(command, cwd=work, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
            if (result.returncode == 0) != expect_success:
                raise AssertionError(f"{command}\n{result.stdout}")
            print(f"{label} ({platform}, {suffix}): {'PASS' if expect_success else 'incompatible as expected'}", flush=True)
            return result.stdout
        run.number = 0
        run("//:selected")
        lock = json.loads((work / "MODULE.bazel.lock").read_text())
        assert any("oci_ref_graph_v2@" + root in values for values in lock["facts"].values()), "graph facts were not persisted"
        requests.clear()
        run("//:selected", fresh=True)
        for arch, child in zip(("amd64", "arm64", "unknown"), children):
            for d in (child["digest"], configs[arch], layers[arch]):
                seen = any(d in path for path in requests)
                assert seen == (arch == "amd64"), (arch, d, requests)
        assert not any(root in path for path in requests), requests
        # Invalidate extension evaluation but retain facts, then switch platform.
        with (work / "MODULE.bazel").open("a") as out:
            out.write('images.pull(name = "second_name", registry = "127.0.0.1:%d", repository = "test", digest = %r)\n' % (server.server_port, root))
        requests.clear()
        run("//:selected", "arm64", fresh=True)
        assert not any(children[0]["digest"] in p or configs["amd64"] in p or layers["amd64"] in p or root in p for p in requests), requests
        requests.clear()
        output = run("//:selected", "windows", fresh=True, expect_success=False)
        assert "incompatible" in output, output
        assert not any("/manifests/" in p or "/blobs/" in p for p in requests), requests
        run("//:original", "windows")
        run("//:arm_app")
        run("//:both")
        run("//:load_original", "windows", extra=["--output_groups=tarball"])
        tarballs = list((run.last_output / "execroot/_main/bazel-out").glob("*/bin/load_original_docker.tar"))
        assert len(tarballs) == 1, tarballs
        with tarfile.open(tarballs[0]) as archive:
            assert archive.extractfile("blobs/sha256/" + root[7:]).read() == manifests[root]
            for child in children:
                assert archive.extractfile("blobs/sha256/" + child["digest"][7:]).read() == manifests[child["digest"]]
        requests.clear()
        run("//:standalone", fresh=True)
        assert not any(d in p for d in layers.values() for p in requests), requests
        output = run("//:standalone", "arm64", expect_success=False)
        assert "incompatible" in output, output
        run("//:standalone_original", "windows")
        for mode in ("shallow", "lazy"):
            module = work / "MODULE.bazel"
            module.write_text(module.read_text().replace('layer_handling = "eager"', 'layer_handling = "%s"' % mode).replace('layer_handling = "shallow"', 'layer_handling = "%s"' % mode))
            requests.clear()
            run("//:selected", fresh=True)
            assert not any(d in p for d in layers.values() for p in requests), requests
            if mode == "lazy":
                requests.clear()
                run("//:app", extra=["--output_groups=mtree", "--action_env=IMG_INSECURE=1", "--sandbox_default_allow_network=true"])
                assert any(layers["amd64"] in p for p in requests), requests
                assert not any(layers[arch] in p for arch in ("arm64", "unknown") for p in requests), requests
        print("All lazy images integration checks passed", flush=True)
    finally:
        server.shutdown()
        # Always stop the test's servers, even when retaining diagnostics.
        for output in work.glob("output-*"):
            subprocess.run([args.bazel, "--nohome_rc", f"--output_base={output}", "shutdown"], cwd=work, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        if not args.keep:
            shutil.rmtree(work)


if __name__ == "__main__":
    main()
