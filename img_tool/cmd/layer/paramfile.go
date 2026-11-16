package layer

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

func readParamFile(paramFile string) (addFiles, error) {
	file, err := os.Open(paramFile)
	if err != nil {
		return nil, fmt.Errorf("opening parameter file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	var files addFiles
	var opts paramFileOptions
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if isDirective, err := applyParamFileDirective(line, &opts); err != nil {
			return nil, fmt.Errorf("parsing parameter file: %w", err)
		} else if isDirective {
			continue
		}
		pathInImage, typeOfFile, file, err := splitParamFileLine(line)
		if err != nil {
			return nil, fmt.Errorf("parsing parameter file: %w", err)
		}
		typ, err := parseFileType(typeOfFile, file)
		if err != nil {
			return nil, fmt.Errorf("parsing parameter file: %w", err)
		}
		files = append(files, addFile{
			PathInImage:      pathInImage,
			File:             file,
			FileType:         typ,
			DedupByContentOK: opts.dedupByContent,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading parameter file: %w", err)
	}
	return files, nil
}

// parseFileType maps the single-character type prefix used in parameter files to
// an api.FileType. Bazel's is_directory attribute doesn't work for source
// directories, so source directories are marked as "f" even though they're
// actually directories; we check the actual filesystem type to handle this.
func parseFileType(typeOfFile, file string) (api.FileType, error) {
	switch typeOfFile {
	case "f":
		if fileInfo, err := os.Stat(file); err == nil && fileInfo.IsDir() {
			return api.Directory, nil
		}
		return api.RegularFile, nil
	case "d":
		return api.Directory, nil
	case "l":
		return api.Symlink, nil
	default:
		return api.FileType{}, fmt.Errorf("invalid type %q", typeOfFile)
	}
}

// paramFileOptions are the settings that directive lines change for the
// entries that follow them in a parameter file.
type paramFileOptions struct {
	// dedupByContent is set by a "dedup=content" directive and cleared by a
	// "dedup=exact" directive: the metadata of the entries in between does not
	// matter, so they may be hardlinked to an earlier regular file with the
	// same content even if their metadata differs. Such an entry is still
	// written with its own metadata when there is no such file. Bazel brackets
	// the files of a runfiles tree with these directives.
	dedupByContent bool
}

// applyParamFileDirective applies a directive line of the form
//
//	\0<key>=<value>[\0<key>=<value>]...
//
// to opts and reports whether line is a directive at all. A path in the image
// cannot be empty, so a line starting with NUL is never a file entry.
func applyParamFileDirective(line string, opts *paramFileOptions) (bool, error) {
	if !strings.HasPrefix(line, "\x00") {
		return false, nil
	}
	for _, kv := range strings.Split(line[1:], "\x00") {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return true, fmt.Errorf("invalid directive %q (want key=value)", kv)
		}
		switch key {
		case "dedup":
			switch value {
			case "content":
				opts.dedupByContent = true
			case "exact":
				opts.dedupByContent = false
			default:
				return true, fmt.Errorf("invalid value %q for directive dedup (want content or exact)", value)
			}
		default:
			return true, fmt.Errorf("unknown directive %q", key)
		}
	}
	return true, nil
}

func splitParamFileLine(line string) (string, string, string, error) {
	// Split the line into three parts: pathInImage, type, and file
	parts := strings.SplitN(line, "\x00", 2)
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid format for line: %s", line)
	}
	pathInImage := parts[0]
	if len(pathInImage) == 0 {
		return "", "", "", fmt.Errorf("path in image cannot be empty: %s", line)
	}
	if pathInImage[0] == '/' {
		return "", "", "", fmt.Errorf("path in image cannot start with '/'. Use %q instead", line[1:])
	}
	rest := parts[1]
	if len(rest) < 2 {
		return "", "", "", fmt.Errorf("invalid format for line: %s", line)
	}
	typeOfFile := rest[:1]
	file := rest[1:]
	if typeOfFile != "f" && typeOfFile != "d" && typeOfFile != "l" {
		return "", "", "", fmt.Errorf("invalid type for line: %s", line)
	}
	return pathInImage, typeOfFile, file, nil
}

func readSymlinkParamFile(paramFile string) (symlinks, error) {
	file, err := os.Open(paramFile)
	if err != nil {
		return nil, fmt.Errorf("opening parameter file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	var links symlinks
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		pathInImage, file, err := splitParamFileLineKV(line)
		if err != nil {
			return nil, fmt.Errorf("parsing parameter file: %w", err)
		}
		links = append(links, symlink{
			LinkName: pathInImage,
			Target:   file,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading parameter file: %w", err)
	}
	return links, nil
}

// splitParamFileLineKV splits a line in the parameter file into key and value.
// This can be used if the file doesn't contain a type character.
func splitParamFileLineKV(line string) (string, string, error) {
	parts := strings.SplitN(line, "\x00", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid format for line: %s", line)
	}
	pathInImage := parts[0]
	if len(pathInImage) == 0 {
		return "", "", fmt.Errorf("path in image cannot be empty: %s", line)
	}
	value := parts[1]
	return pathInImage, value, nil
}

func readEmptyFilesParamFile(paramFile string) ([]string, error) {
	file, err := os.Open(paramFile)
	if err != nil {
		return nil, fmt.Errorf("opening parameter file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	var paths []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if line[0] == '/' {
			line = line[1:]
		}
		paths = append(paths, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading parameter file: %w", err)
	}
	return paths, nil
}

// readBaseMetadataParamFile reads a parameter file listing base metadata stream
// paths, one per line, and returns them in order. Order matters: for a path
// described by several streams, the last one wins.
func readBaseMetadataParamFile(paramFile string) ([]string, error) {
	file, err := os.Open(paramFile)
	if err != nil {
		return nil, fmt.Errorf("opening parameter file: %w", err)
	}
	defer file.Close()

	var paths []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		paths = append(paths, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading parameter file: %w", err)
	}
	return paths, nil
}

// readSymlinkPairsParamFile reads a parameter file where each line contains
// three null-separated fields: source_prefix, dest_prefix, and dir_name.
// For each line, it produces a symlink: source_prefix/dir_name -> dest_prefix/dir_name.
func readSymlinkPairsParamFile(paramFile string) (symlinks, error) {
	file, err := os.Open(paramFile)
	if err != nil {
		return nil, fmt.Errorf("opening parameter file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	var links symlinks
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("expected 3 null-separated fields (source_prefix, dest_prefix, dir_name), got %d in line: %s", len(parts), line)
		}
		srcPrefix := parts[0]
		dstPrefix := parts[1]
		dirName := parts[2]
		if dirName == "" {
			return nil, fmt.Errorf("dir_name cannot be empty: %s", line)
		}
		linkName := srcPrefix + "/" + dirName
		if len(linkName) > 0 && linkName[0] == '/' {
			linkName = linkName[1:]
		}
		links = append(links, symlink{
			LinkName: linkName,
			Target:   dstPrefix + "/" + dirName,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading parameter file: %w", err)
	}
	return links, nil
}

// dedupeSymlinks drops the exact repeats from a list of symlinks, keeping the
// first occurrence of each and the order of the rest.
//
// The symlink pairs of a shared runfiles tree name one link per top-level
// directory, and the caller derives that set from several runfiles components
// (files, symlinks, root symlinks, empty files) that it can only deduplicate one
// at a time. A directory that more than one component names therefore arrives
// more than once. The repeat has to collapse here, because the tar writer
// records every header it is handed: an unfiltered repeat becomes a second,
// identical entry in the layer.
//
// Two links that agree on the name but not on the target are a genuine conflict,
// not a repeat, and both are kept.
func dedupeSymlinks(links symlinks) symlinks {
	seen := make(map[symlink]struct{}, len(links))
	var unique symlinks
	for _, link := range links {
		if _, ok := seen[link]; ok {
			continue
		}
		seen[link] = struct{}{}
		unique = append(unique, link)
	}
	return unique
}
