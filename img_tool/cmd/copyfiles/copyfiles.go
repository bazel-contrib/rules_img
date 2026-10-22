// Command copy-files materializes input files at new paths, using the cheapest
// strategy the filesystem allows: hardlink, else reflink (copy-on-write), else
// a plain byte copy.
//
// Bazel rules use it to turn source files into generated files. A source file
// lives outside the output base, so a relative symlink pointing at it from
// inside a tree artifact is only valid in the execroot of the build that created
// it (see docs: blobs of eagerly pulled base images are source files).
package copyfiles

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/ocilayout"
)

type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return fmt.Sprintf("%v", *s) }

func (s *stringSliceFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func CopyFilesProcess(_ context.Context, args []string) {
	var fromFiles stringSliceFlag

	flagSet := flag.NewFlagSet("copy-files", flag.ExitOnError)
	flagSet.Usage = func() {
		fmt.Fprintf(flagSet.Output(), "Copies files, preferring hardlinks and reflinks over real copies.\n\n")
		fmt.Fprintf(flagSet.Output(), "Source and destination paths alternate: every even-numbered path is a\n")
		fmt.Fprintf(flagSet.Output(), "source, every odd-numbered path the destination it is copied to.\n\n")
		fmt.Fprintf(flagSet.Output(), "Usage: img copy-files [--from-file <paramfile>...] [<src> <dst>...]\n")
		flagSet.PrintDefaults()
	}
	flagSet.Var(&fromFiles, "from-file", "Path to a newline-delimited file listing alternating source and destination paths (repeatable)")

	if err := flagSet.Parse(args); err != nil {
		flagSet.Usage()
		os.Exit(1)
	}

	paths := append([]string{}, flagSet.Args()...)
	for _, paramFile := range fromFiles {
		fromParamFile, err := readParamFile(paramFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading param file %s: %v\n", paramFile, err)
			os.Exit(1)
		}
		paths = append(paths, fromParamFile...)
	}
	if len(paths)%2 != 0 {
		fmt.Fprintf(os.Stderr, "Error: expected an even number of paths (<src> <dst> pairs), got %d\n", len(paths))
		flagSet.Usage()
		os.Exit(1)
	}

	for i := 0; i < len(paths); i += 2 {
		if err := copyOne(paths[i], paths[i+1]); err != nil {
			fmt.Fprintf(os.Stderr, "Error copying %s to %s: %v\n", paths[i], paths[i+1], err)
			os.Exit(1)
		}
	}
}

func copyOne(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	// A hardlink fails on an existing destination, and an action may be retried
	// in a non-empty output directory.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	return ocilayout.CopyFile(src, dst)
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
