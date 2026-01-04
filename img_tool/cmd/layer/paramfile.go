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
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		pathInImage, typeOfFile, file, err := splitParamFileLine(line)
		if err != nil {
			return nil, fmt.Errorf("parsing parameter file: %w", err)
		}
		var typ api.FileType
		switch typeOfFile {
		case "f":
			typ = api.RegularFile
			// Bazel's is_directory attribute doesn't work for source directories,
			// so source directories are marked as "f" in the args file even though
			// they're actually directories. We need to check the actual filesystem
			// type to handle this edge case.
			if fileInfo, err := os.Stat(file); err == nil && fileInfo.IsDir() {
				typ = api.Directory
			}
		case "d":
			typ = api.Directory
		case "l":
			typ = api.Symlink
		default:
			return nil, fmt.Errorf("invalid type for line: %s", line)
		}
		files = append(files, addFile{
			PathInImage: pathInImage,
			File:        file,
			FileType:    typ,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading parameter file: %w", err)
	}
	return files, nil
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
