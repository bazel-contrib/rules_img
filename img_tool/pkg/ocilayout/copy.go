package ocilayout

import (
	"io"
	"os"
	"path/filepath"
)

// openFileBlob opens a file and returns it with its size, for streaming.
func openFileBlob(path string) (io.ReadCloser, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// copyFile materializes src at dst using the most efficient strategy available:
// symlink (when requested), else hardlink, else reflink (copy-on-write on
// btrfs/xfs via FICLONE), else a plain byte copy. This is the ladder moved
// verbatim from cmd/ocilayout (which, unlike the old cmd/dockersave copy,
// includes the reflink step — a strict improvement with byte-identical output).
func copyFile(src, dst string, useSymlinks bool) error {
	if useSymlinks {
		absSrc, err := filepath.Abs(src)
		if err != nil {
			return err
		}
		return os.Symlink(absSrc, dst)
	}

	if err := os.Link(src, dst); err == nil {
		return nil
	}

	if err := tryReflink(src, dst); err == nil {
		return nil
	}

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
