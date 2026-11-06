package destination

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "destination.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseFileSplitsAtTheFirstSlash(t *testing.T) {
	registry, repository, err := ParseFile(writeFile(t, "ghcr.io/acme/app/api\n"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if registry != "ghcr.io" || repository != "acme/app/api" {
		t.Errorf("got %q, %q; want %q, %q", registry, repository, "ghcr.io", "acme/app/api")
	}
}

func TestParseFileRejectsMalformedContent(t *testing.T) {
	for name, content := range map[string]string{
		"empty":            "  \n",
		"multiple lines":   "ghcr.io/a\nghcr.io/b\n",
		"no slash":         "ghcr.io",
		"empty registry":   "/acme/app",
		"empty repository": "ghcr.io/",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseFile(writeFile(t, content)); err == nil {
				t.Errorf("ParseFile(%q) succeeded, want an error", content)
			} else if !strings.Contains(err.Error(), "destination file") {
				t.Errorf("ParseFile(%q) error %q does not name the destination file", content, err)
			}
		})
	}
}
