# Bootable UEFI disk image

This example builds a bootable UEFI disk image with `disk_image`:

| Partition | Type | File system | Content |
|-----------|------|-------------|---------|
| `ESP`     | EFI system partition | FAT32 | `/EFI/BOOT/BOOTX64.EFI`: a unified kernel image |
| `root`    | root, x86-64 | erofs (read-only) | Debian bookworm userspace with systemd as init |
| `var`     | `/var` | ext4 | state that systemd writes |
| `home`    | `/home` | ext4 | writable user data |

## Layout

| Directory | Content |
|-----------|---------|
| `boot/` | the unified kernel image (`uki.bzl`), its os-release and the `ESP` partition |
| `initrd/` | the cpio initramfs |
| `rootfs/` | the erofs root file system and its configuration files |
| `state/` | the writable `var` and `home` partitions |
| `kernel/`, `debian/`, `hello/`, `tools/uki/` | the kernel, the Debian packages, the service and the UKI tool |
| `//:disk` | the disk image, `//:write_disk` writes it |

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
  with udev (`root=PARTLABEL=root`) and switches to it. rules_img packs the files into a
  gzip-compressed cpio archive (`format = "cpio"`).
- **Unified kernel image**: `tools/uki` (pure Go, `boot/uki.bzl`) adds the kernel, initrd,
  command line and os-release as sections to Debian's `linuxx64.efi.stub`
  (from `systemd-boot-efi`).
- **Service**: `hello/` is a statically linked Go program that systemd runs as
  `hello.service`. It prints a message and waits.
- `/etc/fstab` mounts `var` and `home` by partition label. An ext4 layer is only as large
  as its content and cannot be grown online (the image uses `sparse_super2`), so both
  layers use an `ext4_config` with `min_size = "128MiB"`, which leaves the free space for
  the system to write to. The partitions are as large as their file systems.

## Tests

```
bazel test //...                                      # unit tests, the disk image check
bazel test //disktest:boot_test --test_output=streamed  # boots with QEMU + OVMF (manual)
```

`//disktest:disktest_test` reads the finished image: the GPT, the file system of each
partition, and the sections of the unified kernel image. `//disktest:boot_test` needs
`qemu-system-x86_64` and OVMF on the host.

`bazel run //:write_disk -- of=disk.img` writes the disk image to a file or a block device
(`image_dd`). With `--config=compact`, layers are compact streams and the disk image is an
extent stream, so nothing large is stored in the output tree; `write_disk` reconstructs
the bytes when it runs.

To try the image by hand:

```
bazel build //disktest:disk_img
qemu-system-x86_64 -machine q35 -m 2G -nographic \
    -drive if=pflash,format=raw,readonly=on,file=/usr/share/OVMF/OVMF_CODE.fd \
    -drive file=bazel-bin/disk.img,format=raw,if=virtio
```
