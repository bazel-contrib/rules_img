package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type distdirEntry struct {
	basename   string
	sourcePath string
}

type distdirEntries []distdirEntry

func (pb *distdirEntries) String() string {
	return fmt.Sprintf("%v", *pb)
}

func (pb *distdirEntries) Set(value string) error {
	kv := strings.SplitN(value, "=", 2)
	if len(kv) != 2 {
		return fmt.Errorf("invalid value: expected key value pair separated by =")
	}

	*pb = append(*pb, distdirEntry{
		basename:   kv[0],
		sourcePath: kv[1],
	})
	return nil
}

var contents distdirEntries

func main() {
	flag.Var(&contents, "file", "Key-value pairs of basenames to their source path.")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "expected destination dir")
		os.Exit(1)
	}
	distdir := flag.Arg(0)

	if len(contents) == 0 {
		fmt.Fprintln(os.Stderr, "Empty list of contents")
		os.Exit(1)
	}
	if err := os.MkdirAll(distdir, os.ModePerm); err != nil {
		fmt.Fprintf(os.Stderr, "Preparing distdir: %v\n", err)
	}
	for _, entry := range contents {
		if err := copyEntry(distdir, entry); err != nil {
			fmt.Fprintf(os.Stderr, "Copying %s to %s: %v\n", entry.sourcePath, entry.basename, err)
			os.Exit(1)
		}
	}
}

func copyEntry(distdir string, entry distdirEntry) error {
	destPath := filepath.Join(distdir, entry.basename)
	if err := os.Link(entry.sourcePath, destPath); err == nil {
		return nil
	}
	inFile, err := os.Open(entry.sourcePath)
	if err != nil {
		return err
	}
	defer inFile.Close()
	outFile, err := os.Create(destPath)
	if err != nil {
		return err
	}
	_, err = io.Copy(outFile, inFile)
	return err
}
