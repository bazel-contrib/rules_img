package dockersave

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// DockerSaveSink defines the interface for writing Docker save format files
type DockerSaveSink interface {
	// CreateDir creates a directory structure if needed
	CreateDir(path string) error

	// WriteFile writes a file with given data
	WriteFile(path string, data []byte, mode os.FileMode) error

	// CopyFile copies a source file to the destination
	CopyFile(dstPath, srcPath string, useSymlinks bool) error

	// Close finalizes the sink
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

func (d *DirectorySink) CopyFile(dstPath, srcPath string, useSymlinks bool) error {
	fullDstPath := filepath.Join(d.basePath, dstPath)
	return copyFile(srcPath, fullDstPath, useSymlinks)
}

func (d *DirectorySink) Close() error {
	// Nothing to close for directory sink
	return nil
}

// TarSink writes Docker save format to a tar file
type TarSink struct {
	file   *os.File
	writer *tar.Writer
}

// NewTarSink creates a new tar sink
func NewTarSink(tarPath string) (*TarSink, error) {
	if tarPath == "-" {
		// Write to stdout
		writer := tar.NewWriter(os.Stdout)
		return &TarSink{
			file:   nil, // No file to close for stdout
			writer: writer,
		}, nil
	}

	file, err := os.Create(tarPath)
	if err != nil {
		return nil, fmt.Errorf("creating tar file: %w", err)
	}

	writer := tar.NewWriter(file)
	return &TarSink{
		file:   file,
		writer: writer,
	}, nil
}

func (t *TarSink) CreateDir(path string) error {
	// Add trailing slash for directory entries
	if path != "" && path != "." {
		dirPath := filepath.ToSlash(path) + "/"
		header := &tar.Header{
			Name:     dirPath,
			Mode:     0o755,
			Typeflag: tar.TypeDir,
		}
		return t.writer.WriteHeader(header)
	}
	return nil
}

func (t *TarSink) WriteFile(path string, data []byte, mode os.FileMode) error {
	header := &tar.Header{
		Name: filepath.ToSlash(path), // Ensure forward slashes in tar (otherwise Windows paths may cause issues)
		Mode: int64(mode),
		Size: int64(len(data)),
	}

	if err := t.writer.WriteHeader(header); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", path, err)
	}

	_, err := t.writer.Write(data)
	if err != nil {
		return fmt.Errorf("writing tar data for %s: %w", path, err)
	}

	return nil
}

func (t *TarSink) CopyFile(dstPath, srcPath string, useSymlinks bool) error {
	// For tar sink, we can't use symlinks, so we always copy the file content
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening source file %s: %w", srcPath, err)
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return fmt.Errorf("getting file info for %s: %w", srcPath, err)
	}

	header := &tar.Header{
		Name: filepath.ToSlash(dstPath), // Ensure forward slashes in tar (otherwise Windows paths may cause issues)
		Mode: 0o644,
		Size: srcInfo.Size(),
	}

	if err := t.writer.WriteHeader(header); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", dstPath, err)
	}

	_, err = io.Copy(t.writer, srcFile)
	if err != nil {
		return fmt.Errorf("copying file data to tar for %s: %w", dstPath, err)
	}

	return nil
}

func (t *TarSink) Close() error {
	var errs []error

	if err := t.writer.Close(); err != nil {
		errs = append(errs, fmt.Errorf("closing tar writer: %w", err))
	}

	// Only close file if it's not nil (stdout case)
	if t.file != nil {
		if err := t.file.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing tar file: %w", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("errors closing tar sink: %v", errs)
	}

	return nil
}

// copyFile copies a file from src to dst, with options for symlinks and efficient copying
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
