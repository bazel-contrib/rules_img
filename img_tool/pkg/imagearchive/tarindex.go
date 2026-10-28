package imagearchive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/fileopener"
)

// TarEntry records the byte offset and size of a regular file payload inside an uncompressed tar archive.
type TarEntry struct {
	Offset int64
	Size   int64
}

// IndexedTar provides random-access SectionReader views into an uncompressed tar archive without disk extraction.
type IndexedTar struct {
	file    *os.File
	entries map[string]TarEntry
}

// CleanTarPath normalizes an archive entry path and rejects path traversal outside the archive root.
func CleanTarPath(raw string) (string, error) {
	trimmed := strings.TrimLeft(raw, "/")
	cleaned := path.Clean(trimmed)
	if trimmed == "" || cleaned == "." {
		return ".", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("unsafe tar entry path: %s", raw)
	}
	return cleaned, nil
}

// OpenIndexedTar scans the tar headers of tarPath once and records the payload offsets of all entries.
// Because *os.File implements io.Seeker, Go's archive/tar skips file payloads via Seek without reading layer bytes.
func OpenIndexedTar(tarPath string) (*IndexedTar, error) {
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, fmt.Errorf("opening archive %s: %w", tarPath, err)
	}

	if alg, err := fileopener.LearnCompressionAlgorithm(f); err == nil && alg != api.Uncompressed {
		f.Close()
		return nil, fmt.Errorf("archive %s is %s-compressed; pass an uncompressed .tar archive", tarPath, alg)
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, fmt.Errorf("seeking archive %s: %w", tarPath, err)
	}

	entries := make(map[string]TarEntry)
	symlinks := make(map[string]string)

	// Pass f directly without bufio.Reader so archive/tar uses f.Seek to skip payloads
	// and f.Seek(0, io.SeekCurrent) returns the exact start offset of each file body.
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("scanning tar headers in %s: %w", tarPath, err)
		}

		cleanName, err := CleanTarPath(hdr.Name)
		if err != nil {
			f.Close()
			return nil, err
		}
		if cleanName == "." {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			offset, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("recording offset for %s: %w", cleanName, err)
			}
			delete(symlinks, cleanName)
			entries[cleanName] = TarEntry{Offset: offset, Size: hdr.Size}
		case tar.TypeLink:
			// POSIX hardlinks bind to the previously archived entry at the time the header appears.
			target, err := CleanTarPath(hdr.Linkname)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("hardlink %s: %w", cleanName, err)
			}
			delete(symlinks, cleanName)
			if entry, ok := entries[target]; ok {
				entries[cleanName] = entry
			} else if symTarget, ok := symlinks[target]; ok {
				delete(entries, cleanName)
				symlinks[cleanName] = symTarget
			} else {
				delete(entries, cleanName)
			}
		case tar.TypeSymlink:
			if strings.HasPrefix(hdr.Linkname, "/") {
				f.Close()
				return nil, fmt.Errorf("unsafe absolute tar symlink %s -> %s", cleanName, hdr.Linkname)
			}
			joined := path.Clean(path.Join(path.Dir(cleanName), hdr.Linkname))
			if joined == ".." || strings.HasPrefix(joined, "../") {
				f.Close()
				return nil, fmt.Errorf("unsafe tar symlink %s -> %s", cleanName, hdr.Linkname)
			}
			delete(entries, cleanName)
			symlinks[cleanName] = joined
		}
	}

	const maxLinkDepth = 16
	for name := range symlinks {
		curr := name
		for range maxLinkDepth {
			next, isLink := symlinks[curr]
			if !isLink {
				if entry, ok := entries[curr]; ok {
					entries[name] = entry
				}
				break
			}
			curr = next
		}
	}

	return &IndexedTar{file: f, entries: entries}, nil
}

// Close closes the underlying archive file.
func (t *IndexedTar) Close() error {
	if t == nil || t.file == nil {
		return nil
	}
	return t.file.Close()
}

// Has reports whether the archive contains a regular file (or resolved link) at name.
func (t *IndexedTar) Has(name string) bool {
	clean, err := CleanTarPath(name)
	if err != nil {
		return false
	}
	_, ok := t.entries[clean]
	return ok
}

// OpenEntry returns an io.SectionReader bounded to the exact byte range of name in the archive.
func (t *IndexedTar) OpenEntry(name string) (*io.SectionReader, int64, error) {
	clean, err := CleanTarPath(name)
	if err != nil {
		return nil, 0, err
	}
	entry, ok := t.entries[clean]
	if !ok {
		return nil, 0, fmt.Errorf("entry %s not found in archive", clean)
	}
	return io.NewSectionReader(t.file, entry.Offset, entry.Size), entry.Size, nil
}

// ReadBytes reads the full contents of name into memory. Intended for JSON metadata files.
func (t *IndexedTar) ReadBytes(name string) ([]byte, error) {
	sr, _, err := t.OpenEntry(name)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(sr)
}
