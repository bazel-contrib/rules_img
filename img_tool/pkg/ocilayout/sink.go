package ocilayout

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Sink is the single output target for a layout. It merges the two former sink
// interfaces (cmd/ocilayout.OCILayoutSink and cmd/dockersave.DockerSaveSink)
// and absorbs the inline tar writer that used to live in pkg/ocitar.
//
// WriteBlob takes a Blob, so the decision between hardlink, stream and
// in-memory write is made inside the sink — callers stay source-agnostic.
type Sink interface {
	// CreateDir records a directory entry (a tar dir entry, or MkdirAll).
	CreateDir(path string) error
	// WriteFile writes a small in-memory file (marker, index.json,
	// manifest.json, *.descriptor.json).
	WriteFile(path string, data []byte, mode os.FileMode) error
	// WriteBlob writes a blob at path from any Blob source.
	WriteBlob(ctx context.Context, path string, b Blob, opts WriteBlobOptions) error
	// Close finalizes the sink.
	Close() error
}

// WriteBlobOptions carries per-write knobs.
type WriteBlobOptions struct {
	// UseSymlinks makes a directory sink symlink Path blobs instead of copying.
	UseSymlinks bool
	// RequireLink makes a directory sink error (instead of silently copying)
	// when a Path blob cannot be linked or a non-Path blob is given. It keeps
	// hardlink loss loud for callers that depend on links.
	RequireLink bool
	// ProgressFunc, when set, wraps streamed reads with a progress writer. It
	// never affects the bytes written.
	ProgressFunc func(ctx context.Context, size int64, name string) io.Writer
}

const blobMode = os.FileMode(0o644)

// DirectorySink writes a layout to a directory on disk.
type DirectorySink struct {
	basePath string
}

// NewDirectorySink returns a sink that writes under basePath.
func NewDirectorySink(basePath string) *DirectorySink { return &DirectorySink{basePath: basePath} }

func (d *DirectorySink) full(path string) string {
	return filepath.Join(d.basePath, filepath.FromSlash(path))
}

func (d *DirectorySink) CreateDir(path string) error {
	return os.MkdirAll(d.full(path), 0o755)
}

func (d *DirectorySink) WriteFile(path string, data []byte, mode os.FileMode) error {
	full := d.full(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, data, mode)
}

func (d *DirectorySink) WriteBlob(ctx context.Context, path string, b Blob, opts WriteBlobOptions) error {
	if b.isZero() {
		return errNoBlobContent
	}
	dst := d.full(path)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	switch {
	case b.Path != "":
		return copyFile(b.Path, dst, opts.UseSymlinks)
	case opts.RequireLink:
		return fmt.Errorf("ocilayout: blob %s has no file path but a link was required", path)
	case b.Bytes != nil:
		return os.WriteFile(dst, b.Bytes, blobMode)
	default: // streaming Open
		rc, size, err := b.reader(ctx)
		if err != nil {
			return err
		}
		defer rc.Close()
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, blobMode)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(f, progressReader(ctx, rc, size, path, opts.ProgressFunc))
		return err
	}
}

func (d *DirectorySink) Close() error { return nil }

// readFile reads a file relative to the sink's base path (used by the Editor).
func (d *DirectorySink) readFile(path string) ([]byte, error) {
	return os.ReadFile(d.full(path))
}

// blobExists reports whether a blob file already exists with a non-empty size.
func (d *DirectorySink) blobExists(path string) bool {
	info, err := os.Stat(d.full(path))
	return err == nil && !info.IsDir()
}

// TarSink writes a layout as a tar stream to an io.Writer.
type TarSink struct {
	tw     *tar.Writer
	closer io.Closer // optional underlying file to close
}

// NewTarSink writes a tar stream to w (which may be os.Stdout, a *os.File or a
// pipe). The caller owns w's lifecycle unless it is passed via NewTarFileSink.
func NewTarSink(w io.Writer) *TarSink { return &TarSink{tw: tar.NewWriter(w)} }

// NewTarFileSink opens output for writing (or uses stdout when output is "-")
// and returns a TarSink that closes the file on Close.
func NewTarFileSink(output string) (*TarSink, error) {
	if output == "-" {
		return &TarSink{tw: tar.NewWriter(os.Stdout)}, nil
	}
	f, err := os.Create(output)
	if err != nil {
		return nil, fmt.Errorf("creating tar file: %w", err)
	}
	return &TarSink{tw: tar.NewWriter(f), closer: f}, nil
}

func (t *TarSink) CreateDir(path string) error {
	return t.tw.WriteHeader(&tar.Header{
		Name:     filepath.ToSlash(path) + "/",
		Mode:     0o755,
		Typeflag: tar.TypeDir,
	})
}

func (t *TarSink) WriteFile(path string, data []byte, mode os.FileMode) error {
	if err := t.tw.WriteHeader(&tar.Header{
		Name: filepath.ToSlash(path),
		Mode: int64(mode),
		Size: int64(len(data)),
	}); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", path, err)
	}
	_, err := t.tw.Write(data)
	return err
}

func (t *TarSink) WriteBlob(ctx context.Context, path string, b Blob, opts WriteBlobOptions) error {
	if b.isZero() {
		return errNoBlobContent
	}
	name := filepath.ToSlash(path)

	// Path blobs are opened as files; every other kind streams via reader.
	if b.Path != "" {
		rc, size, err := openFileBlob(b.Path)
		if err != nil {
			return err
		}
		defer rc.Close()
		return t.streamEntry(ctx, name, rc, size, path, opts.ProgressFunc)
	}
	if b.Bytes != nil {
		if err := t.tw.WriteHeader(&tar.Header{Name: name, Mode: int64(blobMode), Size: int64(len(b.Bytes))}); err != nil {
			return err
		}
		_, err := t.tw.Write(b.Bytes)
		return err
	}
	rc, size, err := b.reader(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	return t.streamEntry(ctx, name, rc, size, path, opts.ProgressFunc)
}

func (t *TarSink) streamEntry(ctx context.Context, name string, r io.Reader, size int64, progressName string, progress func(ctx context.Context, size int64, name string) io.Writer) error {
	if err := t.tw.WriteHeader(&tar.Header{Name: name, Mode: int64(blobMode), Size: size}); err != nil {
		return fmt.Errorf("writing tar header for %s: %w", name, err)
	}
	if _, err := io.Copy(t.tw, progressReader(ctx, r, size, progressName, progress)); err != nil {
		return fmt.Errorf("copying blob data for %s: %w", name, err)
	}
	return nil
}

func (t *TarSink) Close() error {
	if err := t.tw.Close(); err != nil {
		return fmt.Errorf("closing tar writer: %w", err)
	}
	if t.closer != nil {
		return t.closer.Close()
	}
	return nil
}

// progressReader wraps r with a progress TeeReader when a progress func is set
// and returns a writer for it; otherwise it returns r unchanged. It never
// changes the bytes read.
func progressReader(ctx context.Context, r io.Reader, size int64, name string, progress func(ctx context.Context, size int64, name string) io.Writer) io.Reader {
	if progress == nil {
		return r
	}
	short := name
	if i := len("blobs/sha256/"); len(name) > i {
		short = name[i:]
	}
	if len(short) > 12 {
		short = short[:12]
	}
	pw := progress(ctx, size, short)
	if pw == nil {
		return r
	}
	return io.TeeReader(r, pw)
}
