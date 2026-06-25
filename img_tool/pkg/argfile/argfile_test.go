package argfile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExpandNoArgfile(t *testing.T) {
	args := []string{"--flag", "value", "input.json"}
	got, err := Expand(args)
	if err != nil {
		t.Fatalf("Expand(%v) returned error: %v", args, err)
	}
	if !reflect.DeepEqual(got, args) {
		t.Errorf("Expand(%v) = %v, want %v", args, got, args)
	}
}

func TestExpandOneArgfile(t *testing.T) {
	argfilePath := writeArgfile(t, "--one\nvalue\n")
	args := []string{"before", "@" + argfilePath, "after"}

	got, err := Expand(args)
	if err != nil {
		t.Fatalf("Expand(%v) returned error: %v", args, err)
	}

	want := []string{"before", "--one", "value", "after"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand(%v) = %v, want %v", args, got, want)
	}
}

func TestExpandMultipleArgfilesReturnsError(t *testing.T) {
	first := writeArgfile(t, "first\n")
	second := writeArgfile(t, "second\n")

	_, err := Expand([]string{"@" + first, "@" + second})
	if err == nil {
		t.Fatal("Expand with multiple argfiles returned nil error")
	}
	if !strings.Contains(err.Error(), "multiple argfiles not supported") {
		t.Fatalf("Expand error = %v, want multiple argfiles error", err)
	}
}

func TestExpandIgnoresBlankAndCommentLines(t *testing.T) {
	argfilePath := writeArgfile(t, "\n# comment\n--flag\n\nvalue\n  # another comment  \noutput.json\n")

	got, err := Expand([]string{"@" + argfilePath})
	if err != nil {
		t.Fatalf("Expand returned error: %v", err)
	}

	want := []string{"--flag", "value", "output.json"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Expand() = %v, want %v", got, want)
	}
}

func writeArgfile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "args.params")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing argfile: %v", err)
	}
	return path
}
