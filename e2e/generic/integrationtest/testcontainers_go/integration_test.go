package testcontainers_go

import (
	"os/exec"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

const imageLoaderRunfilesPath = "_main/load/load_image.exe"

func TestLoadImage(t *testing.T) {
	// Locate the image loader binary using runfiles
	imageLoaderPath, err := runfiles.Rlocation(imageLoaderRunfilesPath)
	if err != nil {
		t.Fatalf("Failed to locate image loader: %v", err)
	}

	// exe, err := os.Executable()
	// if err != nil {
	// 	t.Fatalf("Failed to get executable path: %v", err)
	// }
	// os.Setenv("RUNFILES_DIR", exe+".runfiles")

	t.Logf("Image loader path: %s", imageLoaderPath)

	// Execute the image loader to load the image into Docker
	// This is where the issue #377 manifests on macOS:
	// "ERROR: execve failed with code -1"
	// cmd := exec.Command("/run/current-system/sw/bin/strace", "-s", "65536", imageLoaderPath)
	cmd := exec.Command(imageLoaderPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Error loading Docker image: %v\nOutput: %s", err, out)
	}
	t.Logf("Loader output: %s", out)
}
