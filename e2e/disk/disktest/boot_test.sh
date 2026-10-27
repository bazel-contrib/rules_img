#!/usr/bin/env bash
# Boots the disk image with QEMU and UEFI firmware, and waits for the greeting of
# hello.service on the serial console. This needs qemu-system-x86_64 and OVMF on the
# host, so the test is tagged "manual". Run it with:
#
#   bazel test //disktest:boot_test --test_output=streamed
set -euo pipefail

# --- begin runfiles.bash initialization v3 ---
set +e
f=bazel_tools/tools/bash/runfiles/runfiles.bash
source "${RUNFILES_DIR:-/dev/null}/$f" 2>/dev/null ||
  source "$(grep -sm1 "^$f " "${RUNFILES_MANIFEST_FILE:-/dev/null}" | cut -f2- -d' ')" 2>/dev/null ||
  source "$0.runfiles/$f" 2>/dev/null ||
  { echo >&2 "cannot find runfiles.bash"; exit 1; }
set -e
# --- end runfiles initialization ---

disk="$(rlocation "$1")"
timeout_seconds="${BOOT_TIMEOUT:-600}"

firmware=""
for candidate in /usr/share/OVMF/OVMF_CODE.fd /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/ovmf/OVMF.fd /usr/share/edk2/ovmf/OVMF_CODE.fd; do
  if [[ -f "$candidate" ]]; then firmware="$candidate"; break; fi
done
if [[ -z "$firmware" ]] || ! command -v qemu-system-x86_64 >/dev/null; then
  echo "qemu-system-x86_64 and OVMF are required" >&2
  exit 1
fi

# QEMU gets a private copy: the guest writes to /var and /home.
scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
cp --sparse=always "$disk" "$scratch/disk.img"

log="$scratch/serial.log"
qemu-system-x86_64 -machine q35 -cpu max -m 2G -smp 2 -nographic -no-reboot \
  -drive "if=pflash,format=raw,readonly=on,file=$firmware" \
  -drive "file=$scratch/disk.img,format=raw,if=virtio" >"$log" 2>&1 &
qemu=$!

deadline=$((SECONDS + timeout_seconds))
while ((SECONDS < deadline)); do
  # The login prompt is the last thing of a successful boot.
  if grep -q "rules-img-disk login:" "$log"; then
    kill "$qemu" 2>/dev/null || true
    grep -q "Hello from the rules_img disk image!" "$log" || { echo "no greeting of hello.service" >&2; cat "$log"; exit 1; }
    grep -q "Mounted var.mount" "$log" || { echo "/var was not mounted" >&2; cat "$log"; exit 1; }
    grep -q "Mounted home.mount" "$log" || { echo "/home was not mounted" >&2; cat "$log"; exit 1; }
    if grep -q "FAILED" "$log"; then echo "a unit failed" >&2; cat "$log"; exit 1; fi
    echo "PASS: the image booted and hello.service greeted"
    exit 0
  fi
  kill -0 "$qemu" 2>/dev/null || break
  sleep 2
done
echo "FAIL: the image did not boot to a login prompt" >&2
cat "$log" >&2
kill "$qemu" 2>/dev/null || true
exit 1
