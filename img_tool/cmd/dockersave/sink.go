package dockersave

import (
	"io"
	"os"
	"path/filepath"
)

// DockerSaveSink defines the interface for writing Docker save format files
type DockerSaveSink interface {
	CreateDir(path string) error
	WriteFile(path string, data []byte, mode os.FileMode) error
	WriteStream(path string, r io.Reader, mode os.FileMode) error
	CopyFile(dstPath, srcPath string, useSymlinks bool) error
	Close() error
}

// DirectorySink writes Docker save format to a directory
type DirectorySink struct {
	basePath string
}

// NewDirectorySink creates a new directory sink
func NewDirectorySink(basePath string) *DirectorySink {
	return &DirectorySink{basePath: basePath}
}

func (d *DirectorySink) CreateDir(path string) error {
	fullPath := filepath.Join(d.basePath, path)
	return os.MkdirAll(fullPath, 0o755)
}

func (d *DirectorySink) WriteFile(path string, data []byte, mode os.FileMode) error {
	fullPath := filepath.Join(d.basePath, path)
	return os.WriteFile(fullPath, data, mode)
}

// WriteStream writes reconstructed blob bytes without first materializing a source file.
func (d *DirectorySink) WriteStream(path string, r io.Reader, mode os.FileMode) error {
	fullPath := filepath.Join(d.basePath, path)
	dstFile, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, r)
	return err
}

func (d *DirectorySink) CopyFile(dstPath, srcPath string, useSymlinks bool) error {
	fullDstPath := filepath.Join(d.basePath, dstPath)
	return copyFile(srcPath, fullDstPath, useSymlinks)
}

func (d *DirectorySink) Close() error {
	return nil
}

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
