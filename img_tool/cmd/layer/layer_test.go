package layer

import (
	"runtime"
	"testing"
)

func TestRecordedCompressorJobs(t *testing.T) {
	tests := []struct {
		flag string
		want uint8
	}{
		{"1", 1},
		{"0", 1},
		{"", 1},
		{"garbage", 1},
		{"2", 2},
		{"4", 4},
		{"255", 255},
		{"256", 255},    // clamped, must not truncate to 0
		{"257", 255},    // clamped, must not truncate to 1
		{"100000", 255}, // clamped
	}
	for _, tc := range tests {
		if got := recordedCompressorJobs(tc.flag); got != tc.want {
			t.Errorf("recordedCompressorJobs(%q) = %d, want %d", tc.flag, got, tc.want)
		}
	}
}

// TestRecordedCompressorJobsMatchesFactoryDecision verifies the recorded value
// selects the same gzip implementation (pgzip when >1, stdlib otherwise) as the
// compressor the build used. "nproc" and negative values both resolve to NumCPU
// in the compress factory, so the recorded value must be >1 exactly when
// NumCPU > 1 — otherwise reconstruction would pick the other implementation and
// fail the compressed-stream digest check.
func TestRecordedCompressorJobsMatchesFactoryDecision(t *testing.T) {
	n := runtime.NumCPU()
	wantParallel := n > 1
	for _, flag := range []string{"nproc", "-1", "-8"} {
		rec := recordedCompressorJobs(flag)
		if (rec > 1) != wantParallel {
			t.Errorf("recordedCompressorJobs(%q)=%d: parallel=%v, want parallel=%v (NumCPU=%d)",
				flag, rec, rec > 1, wantParallel, n)
		}
	}
}

func TestResolveCompressorJobs(t *testing.T) {
	if got := resolveCompressorJobs("8"); got != 8 {
		t.Errorf("resolveCompressorJobs(\"8\") = %d, want 8", got)
	}
	if got := resolveCompressorJobs("nproc"); got != runtime.NumCPU() {
		t.Errorf("resolveCompressorJobs(\"nproc\") = %d, want %d", got, runtime.NumCPU())
	}
	if got := resolveCompressorJobs("-1"); got != runtime.NumCPU() {
		t.Errorf("resolveCompressorJobs(\"-1\") = %d, want %d (NumCPU)", got, runtime.NumCPU())
	}
	if got := resolveCompressorJobs("bogus"); got != 0 {
		t.Errorf("resolveCompressorJobs(\"bogus\") = %d, want 0", got)
	}
}

func TestCompactStreamCompressionLevel(t *testing.T) {
	// In range: returned verbatim.
	for _, lvl := range []int{-1, 0, 6, 9, 127, -128} {
		got, err := compactStreamCompressionLevel(lvl)
		if err != nil {
			t.Errorf("compactStreamCompressionLevel(%d) unexpected error: %v", lvl, err)
		}
		if int(got) != lvl {
			t.Errorf("compactStreamCompressionLevel(%d) = %d, want %d", lvl, got, lvl)
		}
	}
	// Out of int8 range: hard-fail (must not silently truncate).
	for _, lvl := range []int{128, 129, -129, 1000} {
		if _, err := compactStreamCompressionLevel(lvl); err == nil {
			t.Errorf("compactStreamCompressionLevel(%d): expected error, got nil", lvl)
		}
	}
}
