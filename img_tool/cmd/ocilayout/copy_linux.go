//go:build linux

package ocilayout

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// tryReflink attempts to use FICLONE ioctl for efficient copying on Linux
// This creates a reflink (copy-on-write) copy when supported by the filesystem
func tryReflink(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = srcFile.Close() }()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = dstFile.Close() }()

	// Try FICLONE ioctl for reflink copy
	// FICLONE = 0x40049409
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dstFile.Fd(), 0x40049409, srcFile.Fd())
	if errno == 0 {
		return nil
	}

	// If FICLONE is not supported (e.g., different filesystem, not btrfs/xfs),
	// fall back to regular copy
	if errno == syscall.ENOTSUP || errno == syscall.EXDEV || errno == syscall.EINVAL {
		_, err = io.Copy(dstFile, srcFile)
		return err
	}

	return fmt.Errorf("FICLONE ioctl failed: %v", errno)
}
