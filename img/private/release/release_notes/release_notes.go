package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <tarball_path> <version> <output_file>\n", os.Args[0])
		os.Exit(1)
	}

	tarballPath := os.Args[1]
	version := os.Args[2]
	outputFile := os.Args[3]

	// Calculate SHA256 of the tarball
	sha256Hash, err := calculateSHA256(tarballPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error calculating SHA256: %v\n", err)
		os.Exit(1)
	}

	// Extract filename from path
	tarballName := filepath.Base(tarballPath)

	// Strip 'v' prefix from version if present
	versionWithoutV := strings.TrimPrefix(version, "v")

	// Generate release notes
	releaseNotes, err := generateReleaseNotes(version, versionWithoutV, tarballName, sha256Hash)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating release notes: %v\n", err)
		os.Exit(1)
	}

	// Write to output file
	err = os.WriteFile(outputFile, []byte(releaseNotes), 0644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error writing release notes to %s: %v\n", outputFile, err)
		os.Exit(1)
	}
}

func calculateSHA256(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		// If file doesn't exist, return a placeholder
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Warning: tarball file %s not found, using placeholder SHA256\n", filePath)
			return "SHA256_PLACEHOLDER_FILE_NOT_FOUND", nil
		}
		return "", err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

const releaseNotesTemplate = `## Using Bzlmod

Add the following to your ` + "`" + `MODULE.bazel` + "`" + ` file:

` + "```" + `starlark
bazel_dep(name = "rules_img", version = "{{.VersionWithoutV}}")
` + "```" + `

For further instructions, see the [README](https://github.com/bazel-contrib/rules_img#readme).

## Using WORKSPACE

Add the following to your ` + "`" + `WORKSPACE.bazel` + "`" + ` file:

` + "```" + `starlark
load("@bazel_tools//tools/build_defs/repo:http.bzl", "http_archive")

http_archive(
    name = "rules_img",
    sha256 = "{{.SHA256Hash}}",
    urls = ["https://github.com/bazel-contrib/rules_img/releases/download/{{.Version}}/rules_img-{{.Version}}.tar.gz"],
)

# Load dependencies
load("@rules_img//img:dependencies.bzl", "rules_img_dependencies")
rules_img_dependencies()

# Register prebuilt toolchains
load("@rules_img//img:repositories.bzl", "img_register_prebuilt_toolchains", "pull_tool_register_prebuilt_repositories")
img_register_prebuilt_toolchains()
register_toolchains("@img_toolchain//:all")

# Register prebuilt pull_tool repositories (for pull functionality)
pull_tool_register_prebuilt_repositories()

# Example: Pull a base image
load("@rules_img//img:pull.bzl", "pull")
pull(
    name = "alpine",
    digest = "sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1",
    registry = "index.docker.io",
    repository = "library/alpine",
    tag = "3.22",
)
` + "```" + `

For more examples, see the [README](https://github.com/bazel-contrib/rules_img#readme).
`

type ReleaseData struct {
	Version         string
	VersionWithoutV string
	TarballName     string
	SHA256Hash      string
}

func generateReleaseNotes(version, versionWithoutV, tarballName, sha256Hash string) (string, error) {
	tmpl, err := template.New("release_notes").Parse(releaseNotesTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %v", err)
	}

	data := ReleaseData{
		Version:         version,
		VersionWithoutV: versionWithoutV,
		TarballName:     tarballName,
		SHA256Hash:      sha256Hash,
	}

	var result strings.Builder
	err = tmpl.Execute(&result, data)
	if err != nil {
		return "", fmt.Errorf("failed to execute template: %v", err)
	}

	return result.String(), nil
}
