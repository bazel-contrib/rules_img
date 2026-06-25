package layer

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"strings"
)

// placeFilesSpec describes how to place a target's default outputs into the
// layer. It is read from a parameter file written by Bazel.
//
// The parameter file is self-describing: its first line is a header carrying the
// per-target placement context (which Bazel cannot bake into the per-file lines,
// because the `map_each` callbacks that lazily expand a depset may not capture
// rule context, nor count its elements). Every following line describes a single
// file in the namespace produced by `to_short_path_pair`:
//
//	<rebased_short_path>\0<type_char><source_path>
//
// where <rebased_short_path> is the file's short_path rebased so that main-repo
// files live under "_main/" and external-repo files keep their repository name.
//
// The header line has the form:
//
//	mode\0dest\0anchor\0skip
//
// where:
//   - mode is one of:
//   - "relative": each file is placed at dest joined with the file's path
//     relative to anchor (which may climb out of anchor using ".."). Used for
//     an executable's extra default outputs, anchored at the executable.
//   - "package_relative": like "relative", but if the spec resolves to exactly
//     one file that file is placed directly at dest (the path key is the file
//     path, not a directory).
//   - "flatten": each file is placed directly under dest by basename; but if
//     the spec resolves to exactly one file it is placed directly at dest.
//   - dest is the normalized (no leading slash) destination directory (or, for
//     the single-file collapse, the destination path) in the image.
//   - anchor is the rebased short_path prefix that files are placed relative to.
//     Empty means the image root.
//   - skip is a rebased short_path to omit (the executable, which is added
//     separately). Empty means skip nothing.
//
// Any resolved path that escapes the image root (retains a leading "..") fails.
type placeFilesSpec struct {
	Mode   string
	Dest   string
	Anchor string
	Skip   string
}

type placeFilesEntry struct {
	ShortPath string
	Type      string
	Source    string
}

// readPlaceFilesParamFile reads a placement parameter file and resolves it into
// concrete addFile operations.
func readPlaceFilesParamFile(paramFile string) (addFiles, error) {
	file, err := os.Open(paramFile)
	if err != nil {
		return nil, fmt.Errorf("opening placement parameter file: %w", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Default scanner buffer can be too small for very long lines (deep paths).
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("reading placement parameter file: %w", err)
		}
		// Empty file: nothing to place.
		return nil, nil
	}
	spec, err := parsePlaceFilesHeader(scanner.Text())
	if err != nil {
		return nil, fmt.Errorf("parsing placement parameter file header: %w", err)
	}

	var entries []placeFilesEntry
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		shortPath, typeOfFile, source, err := splitParamFileLine(line)
		if err != nil {
			return nil, fmt.Errorf("parsing placement parameter file: %w", err)
		}
		if spec.Skip != "" && shortPath == spec.Skip {
			continue
		}
		entries = append(entries, placeFilesEntry{ShortPath: shortPath, Type: typeOfFile, Source: source})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading placement parameter file: %w", err)
	}

	var files addFiles
	for _, entry := range entries {
		pathInImage, err := spec.place(entry.ShortPath, len(entries))
		if err != nil {
			return nil, err
		}
		typ, err := parseFileType(entry.Type, entry.Source)
		if err != nil {
			return nil, fmt.Errorf("parsing placement parameter file: %w", err)
		}
		files = append(files, addFile{
			PathInImage: pathInImage,
			File:        entry.Source,
			FileType:    typ,
		})
	}
	return files, nil
}

func parsePlaceFilesHeader(line string) (placeFilesSpec, error) {
	parts := strings.SplitN(line, "\x00", 4)
	if len(parts) != 4 {
		return placeFilesSpec{}, fmt.Errorf("expected 4 null-separated fields (mode, dest, anchor, skip), got %d in line: %q", len(parts), line)
	}
	spec := placeFilesSpec{
		Mode:   parts[0],
		Dest:   strings.TrimPrefix(parts[1], "/"),
		Anchor: parts[2],
		Skip:   parts[3],
	}
	switch spec.Mode {
	case "relative", "package_relative", "flatten":
	default:
		return placeFilesSpec{}, fmt.Errorf("invalid placement mode %q (want \"relative\", \"package_relative\", or \"flatten\")", spec.Mode)
	}
	return spec, nil
}

// place computes the destination path in the image for a file identified by its
// rebased short_path. count is the total number of files the spec resolves to
// (after skips), used by the single-file collapse modes.
func (s placeFilesSpec) place(shortPath string, count int) (string, error) {
	// In the collapse modes, a single output is placed directly at dest (the
	// path key is the file path, not a directory).
	if count == 1 && (s.Mode == "package_relative" || s.Mode == "flatten") {
		return s.Dest, nil
	}
	if s.Mode == "flatten" {
		return path.Join(s.Dest, path.Base(shortPath)), nil
	}
	rel := relPath(shortPath, s.Anchor)
	dest := path.Join(s.Dest, rel)
	// path.Join already cleans the result, but a result that climbs above the
	// image root retains a leading "..".
	if dest == ".." || strings.HasPrefix(dest, "../") {
		return "", fmt.Errorf("file %q would be placed above the image root (resolved to %q); use a deeper path_in_image", shortPath, dest)
	}
	return dest, nil
}

// relPath returns target expressed relative to base, emitting ".." segments to
// climb out of base when target is not beneath it. Both arguments are relative
// slash-separated paths in the same namespace.
func relPath(target, base string) string {
	targetSegs := splitSegments(target)
	baseSegs := splitSegments(base)
	common := 0
	for common < len(targetSegs) && common < len(baseSegs) && targetSegs[common] == baseSegs[common] {
		common++
	}
	var segs []string
	for i := common; i < len(baseSegs); i++ {
		segs = append(segs, "..")
	}
	segs = append(segs, targetSegs[common:]...)
	if len(segs) == 0 {
		return "."
	}
	return strings.Join(segs, "/")
}

func splitSegments(p string) []string {
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s == "" || s == "." {
			continue
		}
		segs = append(segs, s)
	}
	return segs
}
