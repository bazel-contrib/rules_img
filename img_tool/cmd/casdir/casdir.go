// Command cas-dir builds a content-addressed directory from a set of input
// files. For every regular file (following symlinks; directories are walked)
// it writes the file's content to <output>/sha256/<sha256 hex of content>,
// deduplicating identical content. Symlinks and unreadable entries are skipped —
// they carry no content blob.
//
// With --symlink, entries are relative symlinks to the input files instead of
// copies of their content, so the inputs are not materialized a second time.
//
// The resulting directory is used to reconstruct a layer tar from its CAS stream
// index: each CAS reference (addressed by the sha256 of its content) is resolved
// by opening <dir>/sha256/<hex>.
package casdir

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return fmt.Sprintf("%v", *s) }

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func CASDirProcess(_ context.Context, args []string) {
	var outputDir string
	var fromFiles stringSliceFlag
	var useSymlinks bool

	flagSet := flag.NewFlagSet("cas-dir", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Builds a content-addressed directory (sha256/<hex>) from input files.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img cas-dir --output <dir> [--from-file <paramfile>...] [file...]\n")
		flagSet.PrintDefaults()
	}
	flagSet.StringVar(&outputDir, "output", "", "Output directory for the content-addressed store (required)")
	flagSet.Var(&fromFiles, "from-file", "Path to a newline-delimited file listing input files/directories (repeatable)")
	flagSet.BoolVar(&useSymlinks, "symlink", false, "Use symlinks instead of copying files")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}
	if outputDir == "" {
		fmt.Fprintf(os.Stderr, "Error: --output is required\n")
		flagSet.Usage()
		os.Exit(1)
	}

	inputs := append([]string{}, flagSet.Args()...)
	for _, paramFile := range fromFiles {
		paths, err := readParamFile(paramFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading param file %s: %v\n", paramFile, err)
			os.Exit(1)
		}
		inputs = append(inputs, paths...)
	}

	w := &casWriter{shaDir: filepath.Join(outputDir, "sha256"), useSymlinks: useSymlinks}
	if err := os.MkdirAll(w.shaDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "Error creating output directory: %v\n", err)
		os.Exit(1)
	}
	for _, p := range inputs {
		if err := w.addPath(p); err != nil {
			fmt.Fprintf(os.Stderr, "Error adding %s: %v\n", p, err)
			os.Exit(1)
		}
	}
}

type casWriter struct {
	shaDir      string
	useSymlinks bool
}

// addPath adds a file, or every regular file within a directory, to the store.
// Symlinks are followed; dangling/unreadable entries (e.g. native unresolved
// symlinks) carry no content blob and are skipped.
func (w *casWriter) addPath(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		return nil // dangling/unresolved symlink or missing path: nothing to store
	}
	if info.IsDir() {
		return filepath.WalkDir(p, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			fi, statErr := os.Stat(path)
			if statErr != nil || !fi.Mode().IsRegular() {
				return nil
			}
			return w.addFile(path)
		})
	}
	if info.Mode().IsRegular() {
		return w.addFile(p)
	}
	return nil
}

// addFile stores the file under its content-addressed name, either as a relative
// symlink to it or as a copy of its content.
func (w *casWriter) addFile(filePath string) error {
	if w.useSymlinks {
		return w.linkFile(filePath)
	}
	return w.copyFile(filePath)
}

// linkFile hashes the file's content and places a relative symlink to it at its
// content-addressed name, so the content is referenced instead of duplicated.
// The target must be relative: the store is a Bazel tree artifact, and only
// relative symlinks stay valid wherever the tree is staged (sandbox, remote
// execution, runfiles tree).
func (w *casWriter) linkFile(filePath string) error {
	src, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", filePath, err)
	}
	h := sha256.New()
	_, err = io.Copy(h, src)
	src.Close()
	if err != nil {
		return fmt.Errorf("hashing %s: %w", filePath, err)
	}

	dest := filepath.Join(w.shaDir, hex.EncodeToString(h.Sum(nil)))
	if _, err := os.Lstat(dest); err == nil {
		// Already stored (deduplicated).
		return nil
	}
	target, err := relativeTarget(filePath, dest)
	if err != nil {
		return fmt.Errorf("relative path from %s to %s: %w", dest, filePath, err)
	}
	return os.Symlink(target, dest)
}

// relativeTarget returns src as a path relative to the directory holding dst.
func relativeTarget(src, dst string) (string, error) {
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return "", err
	}
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return "", err
	}
	return filepath.Rel(filepath.Dir(absDst), absSrc)
}

// copyFile streams the file once through a hash and a temp file, then renames the
// temp file to its content-addressed name. Identical content is deduplicated.
func (w *casWriter) copyFile(filePath string) error {
	src, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", filePath, err)
	}
	defer src.Close()

	tmp, err := os.CreateTemp(w.shaDir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), src); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("hashing %s: %w", filePath, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing temp file: %w", err)
	}

	dest := filepath.Join(w.shaDir, hex.EncodeToString(h.Sum(nil)))
	if _, err := os.Stat(dest); err == nil {
		// Already stored (deduplicated).
		return os.Remove(tmpName)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("renaming to %s: %w", dest, err)
	}
	return nil
}

func readParamFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var paths []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		paths = append(paths, line)
	}
	return paths, scanner.Err()
}
