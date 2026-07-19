package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	excludeDirective = "# gazelle:exclude_from_release"
)

func main() {
	var (
		repoRoot        = flag.String("repo_root", os.Getenv("BUILD_WORKSPACE_DIRECTORY"), "Repository root directory")
		releaseFilePath = flag.String("release_file", "img/private/release/source_files/BUILD.bazel", "Path to release files BUILD.bazel")
		testMode        = flag.Bool("test", false, "Test mode: check if the list is up to date without modifying it")
	)

	flag.Parse()
	if *repoRoot == "" {
		*repoRoot = "."
	}

	// Find all packages with BUILD files
	packages, err := findPackages(*repoRoot)
	if err != nil {
		log.Fatalf("Failed to find packages: %v", err)
	}

	releaseFileFullPath := filepath.Join(*repoRoot, *releaseFilePath)

	if *testMode {
		// Test mode: check if the list is up to date
		if err := checkReleaseFiles(releaseFileFullPath, packages, *repoRoot); err != nil {
			log.Fatalf("Release files are not up to date: %v", err)
		}
		fmt.Printf("Release files are up to date with %d packages\n", len(packages))
	} else {
		// Update mode: update the release files
		if err := updateReleaseFiles(releaseFileFullPath, packages, *repoRoot); err != nil {
			log.Fatalf("Failed to update release files: %v", err)
		}
		fmt.Printf("Updated release files with %d packages\n", len(packages))
	}
}

// findPackages walks the repository and finds all packages that should be included
func findPackages(repoRoot string) ([]string, error) {
	var packages []string

	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip hidden directories and bazel output directories
		if info.IsDir() {
			name := info.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "bazel-") {
				return filepath.SkipDir
			}
		}

		// Look for BUILD.bazel files
		if info.Name() == "BUILD.bazel" || info.Name() == "BUILD" {
			// Get package path relative to repo root
			rel, err := filepath.Rel(repoRoot, filepath.Dir(path))
			if err != nil {
				return err
			}

			// Normalize to forward slashes
			rel = filepath.ToSlash(rel)
			if rel == "." {
				rel = ""
			}

			// Check if excluded
			if !isExcludedFromRelease(path) {
				packages = append(packages, rel)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sort packages for consistent output
	sort.Strings(packages)
	return packages, nil
}

// isExcludedFromRelease checks if a BUILD file contains the exclude directive
func isExcludedFromRelease(buildFilePath string) bool {
	file, err := os.Open(buildFilePath)
	if err != nil {
		return false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == excludeDirective {
			return true
		}
	}

	return false
}

// hasAllFilesTarget checks if a BUILD file contains an "all_files" target
func hasAllFilesTarget(buildFilePath string) bool {
	file, err := os.Open(buildFilePath)
	if err != nil {
		return false
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// Check for filegroup or other rule named "all_files"
		if strings.Contains(line, `name = "all_files"`) ||
			strings.Contains(line, `name="all_files"`) ||
			strings.Contains(line, `"all_files"`) && strings.Contains(line, "name") {
			return true
		}
	}

	return false
}

// checkReleaseFiles checks if the release_files list matches the expected packages
func checkReleaseFiles(releaseFilePath string, packages []string, repoRoot string) error {
	// Get sorted list of packages that have all_files target
	var expectedPackages []string
	for _, pkg := range packages {
		// Construct the path to the BUILD file
		buildFilePath := filepath.Join(repoRoot, pkg, "BUILD.bazel")
		if _, err := os.Stat(buildFilePath); os.IsNotExist(err) {
			// Try BUILD if BUILD.bazel doesn't exist
			buildFilePath = filepath.Join(repoRoot, pkg, "BUILD")
		}

		// Only include if the BUILD file contains an all_files target
		if hasAllFilesTarget(buildFilePath) {
			expectedPackages = append(expectedPackages, pkg)
		}
	}
	sort.Strings(expectedPackages)

	// Read the existing file
	content, err := os.ReadFile(releaseFilePath)
	if err != nil {
		return fmt.Errorf("failed to read release files: %w", err)
	}

	// Extract current package list from the file
	currentPackages, err := extractPackagesFromContent(string(content))
	if err != nil {
		return fmt.Errorf("failed to extract packages: %w", err)
	}

	// Compare the lists
	if !slicesEqual(currentPackages, expectedPackages) {
		fmt.Fprintln(os.Stderr, "\033[31mRelease files list is out of date!\033[0m")
		fmt.Fprintln(os.Stderr, "  bazel run //img/private/release/gazelle_plugin/cmd/update_release_files")
		return fmt.Errorf("old and new lists don't match.")
	}

	return nil
}

// extractPackagesFromContent extracts package names from BUILD file content
func extractPackagesFromContent(content string) ([]string, error) {
	var packages []string
	lines := strings.Split(content, "\n")
	inReleaseFiles := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if strings.Contains(line, "release_files = [") {
			inReleaseFiles = true
			continue
		}

		if inReleaseFiles && trimmed == "]" {
			break
		}

		if inReleaseFiles && strings.HasPrefix(trimmed, "\"//") && strings.HasSuffix(trimmed, ":all_files\",") {
			// Extract package name
			pkg := strings.TrimPrefix(trimmed, "\"//")
			pkg = strings.TrimSuffix(pkg, ":all_files\",")
			packages = append(packages, pkg)
		}
	}

	return packages, nil
}

// slicesEqual compares two string slices
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// updateReleaseFiles updates the release_files list in the BUILD file
func updateReleaseFiles(releaseFilePath string, packages []string, repoRoot string) error {
	// Read the existing file
	content, err := os.ReadFile(releaseFilePath)
	if err != nil {
		return fmt.Errorf("failed to read release files: %w", err)
	}

	// Find the release_files list boundaries
	lines := strings.Split(string(content), "\n")
	startIdx := -1
	endIdx := -1

	for i, line := range lines {
		if strings.Contains(line, "release_files = [") {
			startIdx = i
		}
		if startIdx >= 0 && strings.TrimSpace(line) == "]" {
			endIdx = i
			break
		}
	}

	if startIdx < 0 || endIdx < 0 {
		return fmt.Errorf("could not find release_files list in %s", releaseFilePath)
	}

	// Build new content
	var newLines []string

	// Add lines before release_files
	newLines = append(newLines, lines[:startIdx+1]...)

	// Add sorted package entries, but only if they have all_files target
	for _, pkg := range packages {
		// Construct the path to the BUILD file
		buildFilePath := filepath.Join(repoRoot, pkg, "BUILD.bazel")
		if _, err := os.Stat(buildFilePath); os.IsNotExist(err) {
			// Try BUILD if BUILD.bazel doesn't exist
			buildFilePath = filepath.Join(repoRoot, pkg, "BUILD")
		}

		// Only add if the BUILD file contains an all_files target
		if hasAllFilesTarget(buildFilePath) {
			target := fmt.Sprintf("    \"//%s:all_files\",", pkg)
			if pkg == "" {
				target = "    \"//:all_files\","
			}
			newLines = append(newLines, target)
		}
	}

	// Add lines after release_files
	newLines = append(newLines, lines[endIdx:]...)

	// Write the updated file
	output := strings.Join(newLines, "\n")
	if err := os.WriteFile(releaseFilePath, []byte(output), 0644); err != nil {
		return fmt.Errorf("failed to write release files: %w", err)
	}

	return nil
}
