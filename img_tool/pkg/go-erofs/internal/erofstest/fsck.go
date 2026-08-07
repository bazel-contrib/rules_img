package erofstest

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
)

// FsckErofs runs fsck.erofs on the image at path. Skips if fsck.erofs
// is not installed. Calls t.Error on validation failure.
func FsckErofs(t testing.TB, path string) {
	t.Helper()
	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		return // silently skip
	}
	cmd := exec.Command("fsck.erofs", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("fsck.erofs %s failed: %v\n%s", path, err, out)
	}
}

// FsckErofsBytes writes buf to a temp file, runs fsck.erofs, and cleans up.
func FsckErofsBytes(t testing.TB, buf []byte) {
	t.Helper()
	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		return
	}
	f, err := os.CreateTemp(t.TempDir(), "fsck-*.erofs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("fsck.erofs", f.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("fsck.erofs failed: %v\n%s", err, out)
	}
}

// FsckAvailable returns true if fsck.erofs is on PATH.
func FsckAvailable() bool {
	_, err := exec.LookPath("fsck.erofs")
	return err == nil
}

// RequireFsck skips t if fsck.erofs is not available.
func RequireFsck(t testing.TB) {
	t.Helper()
	if !FsckAvailable() {
		t.Skip("fsck.erofs not available")
	}
}

// FsckErofsDevice runs fsck.erofs with --device to validate metadata-only
// images that reference external blob devices.
func FsckErofsDevice(t testing.TB, imagePath string, devicePaths ...string) {
	t.Helper()
	if _, err := exec.LookPath("fsck.erofs"); err != nil {
		return
	}
	args := []string{imagePath}
	for _, d := range devicePaths {
		args = append(args, fmt.Sprintf("--device=%s", d))
	}
	cmd := exec.Command("fsck.erofs", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("fsck.erofs %s failed: %v\n%s", imagePath, err, out)
	}
}
