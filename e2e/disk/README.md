# Bootable UEFI disk image

This example builds a bootable UEFI disk image with `disk_image`:

| Partition | Type | File system | Content |
|-----------|------|-------------|---------|
| `ESP`     | EFI system partition | FAT32 | `/EFI/BOOT/BOOTX64.EFI`: a unified kernel image |
| `root`    | root, x86-64 | erofs (read-only) | Debian bookworm userspace with systemd as init |
| `var`     | `/var` | ext4 | state that systemd writes |
| `home`    | `/home` | ext4 | writable user data |

## Pieces

- **Kernel**: built from source by [`linux.bzl`](https://github.com/hermeticbuild/linux.bzl)
  with the hermetic LLVM toolchain (`@llvm`). The config is in `kernel/kernel.config`.
  The kernel graph compiles tools for the *build host* (`objtool`, ...), which are Linux
  programs, so the build needs a Linux host or remote execution (a Linux VM works).
- **Userspace**: Debian packages (`systemd`, `udev`, glibc, ...) from
  [`rules_distroless`](https://github.com/bazel-contrib/rules_distroless). The packages
  are pinned in `debian/apt.lock.json`; regenerate it with
  `bazel run @disk_debian//:lock` after changing `debian/apt.yaml`.
- **Initrd**: the same systemd, with `/init` linked to it. It finds the root partition
  with udev (`root=PARTLABEL=root`) and switches to it. `tools/tar2cpio` (pure Go)
  packs the files into a cpio archive.
- **Unified kernel image**: `tools/uki` (pure Go, `uki.bzl`) adds the kernel, initrd,
  command line and os-release as sections to Debian's `linuxx64.efi.stub`
  (from `systemd-boot-efi`).
- **Service**: `hello/` is a statically linked Go program that systemd runs as
  `hello.service`. It prints a message and waits.
- `/etc/fstab` mounts `var` and `home` by partition label. An ext4 layer is only as large
  as its content and cannot be grown online (the image uses `sparse_super2`), so each of
  the two layers carries a 40 MiB placeholder file that `tmpfiles.d` removes at boot,
  which leaves free space for the system to write to.

## Tests

```
bazel test //...                                      # unit tests, the disk image check
bazel test //disktest:boot_test --test_output=streamed  # boots with QEMU + OVMF (manual)
```

`//disktest:disktest_test` reads the finished image: the GPT, the file system of each
partition, and the sections of the unified kernel image. `//disktest:boot_test` needs
`qemu-system-x86_64` and OVMF on the host.

To try the image by hand:

```
bazel build //disktest:disk_img
qemu-system-x86_64 -machine q35 -m 2G -nographic \
    -drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE.fd \
    -drive file=bazel-bin/disk.img,format=raw,if=virtio
```
