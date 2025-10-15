package credential

import (
	"os"
	"testing"
)

func TestNew_ReplacesWorkspacePlaceholder(t *testing.T) {
	// Set up environment variable
	orig := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	defer func() {
		if err := os.Setenv("BUILD_WORKSPACE_DIRECTORY", orig); err != nil {
			t.Logf("Error restoring BUILD_WORKSPACE_DIRECTORY: %v", err)
		}
	}()
	if err := os.Setenv("BUILD_WORKSPACE_DIRECTORY", "/tmp/workspace"); err != nil {
		t.Fatalf("Error setting BUILD_WORKSPACE_DIRECTORY: %v", err)
	}

	helper := New("%workspace%/bin/helper")
	extHelper, ok := helper.(*externalCredentialHelper)
	if !ok {
		t.Fatalf("expected *externalCredentialHelper, got %T", helper)
	}
	expected := "/tmp/workspace/bin/helper"
	if extHelper.helperBinary != expected {
		t.Errorf("expected helperBinary to be %q, got %q", expected, extHelper.helperBinary)
	}
}

func TestNew_WithoutWorkspacePlaceholder(t *testing.T) {
	orig := os.Getenv("BUILD_WORKSPACE_DIRECTORY")
	defer func() {
		if err := os.Setenv("BUILD_WORKSPACE_DIRECTORY", orig); err != nil {
			t.Logf("Error restoring BUILD_WORKSPACE_DIRECTORY: %v", err)
		}
	}()
	if err := os.Setenv("BUILD_WORKSPACE_DIRECTORY", "/tmp/workspace"); err != nil {
		t.Fatalf("Error setting BUILD_WORKSPACE_DIRECTORY: %v", err)
	}

	helper := New("/usr/local/bin/helper")
	extHelper, ok := helper.(*externalCredentialHelper)
	if !ok {
		t.Fatalf("expected *externalCredentialHelper, got %T", helper)
	}
	expected := "/usr/local/bin/helper"
	if extHelper.helperBinary != expected {
		t.Errorf("expected helperBinary to be %q, got %q", expected, extHelper.helperBinary)
	}
}
