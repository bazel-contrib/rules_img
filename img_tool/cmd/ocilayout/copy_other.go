//go:build !linux

package ocilayout

import (
	"io"
	"os"
)

// tryReflink is not supported on non-Linux platforms, so it just does a regular copy
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

	_, err = io.Copy(dstFile, srcFile)
	return err
}
