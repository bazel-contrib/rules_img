//go:build linux

package ocilayout

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// tryReflink attempts a copy-on-write clone via the FICLONE ioctl, falling back
// to a regular copy when the filesystem does not support it. Moved verbatim
// from cmd/ocilayout.
func tryReflink(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	// FICLONE = 0x40049409
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dstFile.Fd(), 0x40049409, srcFile.Fd())
	if errno == 0 {
		return nil
	}

	if errno == syscall.ENOTSUP || errno == syscall.EXDEV || errno == syscall.EINVAL {
		_, err = io.Copy(dstFile, srcFile)
		return err
	}

	return fmt.Errorf("FICLONE ioctl failed: %v", errno)
}
