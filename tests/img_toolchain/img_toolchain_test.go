package img_toolchain

import (
	"context"
	"flag"
	"path/filepath"
	"testing"
)

var verbose = flag.Bool("verbose", false, "enable verbose output for failed assertions")

func TestImgToolchain(t *testing.T) {
	ctx := context.Background()

	tf, err := NewTestFramework(t)
	if err != nil {
		t.Fatalf("Failed to create test framework: %v", err)
	}
	tf.Verbose = *verbose
	defer tf.Cleanup()

	testFiles, err := filepath.Glob("testcases/*.ini")
	if err != nil {
		t.Fatalf("Failed to find test files: %v", err)
	}

	if len(testFiles) == 0 {
		t.Skip("No test files found in testcases/")
	}

	for _, testFile := range testFiles {
		testCase, err := tf.LoadTestCase(testFile)
		if err != nil {
			t.Fatalf("Failed to load test case %s: %v", testFile, err)
		}

		if err := tf.RunTestCase(ctx, testCase); err != nil {
			t.Errorf("Test case %s failed: %v", testFile, err)
		}
	}
}
