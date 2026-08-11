package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bazelbuild/buildtools/build"
)

// setModuleVersion rewrites the version of the module() call in a MODULE.bazel
// file.
//
// The version is located with a Starlark parser rather than a regular
// expression -- MODULE.bazel files carry more than one `version = ` and the one
// that matters is the keyword argument of module() -- but only the bytes of that
// one string literal are replaced. Reformatting the file with the parser's
// printer instead would turn a one-line change into a diff over anything whose
// formatting the printer disagrees with.
func setModuleVersion(content []byte, version string) ([]byte, error) {
	file, err := parseModuleBazel(content)
	if err != nil {
		return nil, err
	}

	literal, err := moduleVersionLiteral(file)
	if err != nil {
		return nil, err
	}

	start, end := literal.Start.Byte, literal.End.Byte
	if start < 0 || end <= start || end > len(content) {
		return nil, fmt.Errorf("module() version literal has no usable position in the source")
	}
	patched := make([]byte, 0, len(content)+len(version))
	patched = append(patched, content[:start]...)
	patched = append(patched, strconv.Quote(version)...)
	patched = append(patched, content[end:]...)

	// The splice relies on the parser's byte offsets, so confirm the result says
	// what it should before it is published.
	check, err := parseModuleBazel(patched)
	if err != nil {
		return nil, fmt.Errorf("patched MODULE.bazel does not parse: %w", err)
	}
	checkLiteral, err := moduleVersionLiteral(check)
	if err != nil {
		return nil, fmt.Errorf("patched MODULE.bazel: %w", err)
	}
	if checkLiteral.Value != version {
		return nil, fmt.Errorf("patched MODULE.bazel declares version %q, want %q", checkLiteral.Value, version)
	}
	return patched, nil
}

func parseModuleBazel(content []byte) (*build.File, error) {
	file, err := build.ParseModule("MODULE.bazel", content)
	if err != nil {
		return nil, fmt.Errorf("parsing MODULE.bazel: %w", err)
	}
	return file, nil
}

// moduleVersionLiteral returns the string literal assigned to the version
// keyword argument of the file's module() call.
func moduleVersionLiteral(file *build.File) (*build.StringExpr, error) {
	for _, statement := range file.Stmt {
		call, ok := statement.(*build.CallExpr)
		if !ok {
			continue
		}
		if identifier, ok := call.X.(*build.Ident); !ok || identifier.Name != "module" {
			continue
		}
		for _, argument := range call.List {
			assignment, ok := argument.(*build.AssignExpr)
			if !ok {
				continue
			}
			if lhs, ok := assignment.LHS.(*build.Ident); !ok || lhs.Name != "version" {
				continue
			}
			literal, ok := assignment.RHS.(*build.StringExpr)
			if !ok {
				return nil, fmt.Errorf("module() version is not a string literal")
			}
			return literal, nil
		}
		return nil, fmt.Errorf("module() has no version keyword argument")
	}
	return nil, fmt.Errorf("no module() call found in MODULE.bazel")
}

const diffContext = 3

// unifiedDiff produces a `patch -p1` compatible diff of a single file, as Bazel
// applies it to a module's source archive.
//
// The patch keeps the archive's MODULE.bazel in agreement with the copy this
// registry serves, which is the one Bazel resolves against.
func unifiedDiff(filename string, original, modified []byte) ([]byte, error) {
	oldLines := strings.Split(string(original), "\n")
	newLines := strings.Split(string(modified), "\n")

	// The extent of the change: everything between the common prefix and the
	// common suffix.
	start := 0
	for start < len(oldLines) && start < len(newLines) && oldLines[start] == newLines[start] {
		start++
	}
	oldEnd, newEnd := len(oldLines), len(newLines)
	for oldEnd > start && newEnd > start && oldLines[oldEnd-1] == newLines[newEnd-1] {
		oldEnd--
		newEnd--
	}
	if start == oldEnd && start == newEnd {
		return nil, fmt.Errorf("%s: nothing to patch", filename)
	}

	hunkStart := max(start-diffContext, 0)
	oldHunkEnd := min(oldEnd+diffContext, len(oldLines))
	newHunkEnd := newEnd + (oldHunkEnd - oldEnd)

	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n", filename, filename)
	fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n",
		hunkStart+1, oldHunkEnd-hunkStart,
		hunkStart+1, newHunkEnd-hunkStart,
	)
	for _, line := range oldLines[hunkStart:start] {
		fmt.Fprintf(&out, " %s\n", line)
	}
	for _, line := range oldLines[start:oldEnd] {
		fmt.Fprintf(&out, "-%s\n", line)
	}
	for _, line := range newLines[start:newEnd] {
		fmt.Fprintf(&out, "+%s\n", line)
	}
	for _, line := range oldLines[oldEnd:oldHunkEnd] {
		fmt.Fprintf(&out, " %s\n", line)
	}

	diff := []byte(out.String())
	if err := verifyPatch(original, modified, diff); err != nil {
		return nil, err
	}
	return diff, nil
}

// verifyPatch applies diff to original and reports whether the result is
// modified. A patch that does not apply breaks the fetch of every consumer, so
// it is checked here rather than discovered there.
func verifyPatch(original, modified, diff []byte) error {
	applied, err := applyUnifiedDiff(original, diff)
	if err != nil {
		return fmt.Errorf("verifying generated patch: %w", err)
	}
	if !bytesEqual(applied, modified) {
		return fmt.Errorf("verifying generated patch: applying it does not reproduce the patched file")
	}
	return nil
}

// applyUnifiedDiff applies a single-file, single-hunk unified diff.
func applyUnifiedDiff(original, diff []byte) ([]byte, error) {
	lines := strings.Split(strings.TrimSuffix(string(diff), "\n"), "\n")
	if len(lines) < 3 {
		return nil, fmt.Errorf("patch is too short")
	}
	header := lines[2]
	var oldStart, oldCount, newStart, newCount int
	if _, err := fmt.Sscanf(header, "@@ -%d,%d +%d,%d @@", &oldStart, &oldCount, &newStart, &newCount); err != nil {
		return nil, fmt.Errorf("parsing hunk header %q: %w", header, err)
	}

	oldLines := strings.Split(string(original), "\n")
	if oldStart < 1 || oldStart-1+oldCount > len(oldLines) {
		return nil, fmt.Errorf("hunk at line %d spans past the end of the file", oldStart)
	}

	out := append([]string{}, oldLines[:oldStart-1]...)
	cursor := oldStart - 1
	for _, line := range lines[3:] {
		if len(line) == 0 {
			return nil, fmt.Errorf("empty line in hunk body")
		}
		body := line[1:]
		switch line[0] {
		case ' ', '-':
			if cursor >= len(oldLines) || oldLines[cursor] != body {
				return nil, fmt.Errorf("hunk does not match the file at line %d", cursor+1)
			}
			cursor++
			if line[0] == ' ' {
				out = append(out, body)
			}
		case '+':
			out = append(out, body)
		default:
			return nil, fmt.Errorf("unexpected line %q in hunk body", line)
		}
	}
	if cursor != oldStart-1+oldCount {
		return nil, fmt.Errorf("hunk consumed %d lines, header says %d", cursor-oldStart+1, oldCount)
	}
	out = append(out, oldLines[cursor:]...)
	return []byte(strings.Join(out, "\n")), nil
}

func bytesEqual(a, b []byte) bool {
	return string(a) == string(b)
}
