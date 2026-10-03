package layer

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/contentmanifest"
)

func TestTreeMetadata(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("same content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, handling := range []string{"full", "deduplicate_symlink"} {
		t.Run(handling, func(t *testing.T) {
			metadata, err := ParseLayerMetadata(
				`{"mode":"0644","uid":42,"gid":43,"mtime":"2023-01-01T08:00:00Z"}`,
				map[string]string{
					"app/first/b":  `{"mode":"0600"}`,
					"app/second/a": `{"mode":"0700"}`,
					"links":        `{"mode":"0750","uid":44}`,
					"links/alias":  `{"mode":"0777"}`,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			_, err = handleLayerState(api.Uncompressed, false, addFiles{
				{PathInImage: "app/first", File: dir, FileType: api.Directory},
				{PathInImage: "app/second", File: dir, FileType: api.Directory},
			}, nil, nil, symlinks{{LinkName: "links/alias", Target: "../app/first/a"}}, nil, nil,
				contentmanifest.NewMultiImporter(nil, api.SHA256), contentmanifest.NopExporter(), &out, metadata,
				"1", -1, true, handling, "", 0)
			if err != nil {
				t.Fatal(err)
			}
			headers := map[string]*tar.Header{}
			reader := tar.NewReader(&out)
			for {
				header, err := reader.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				headers[strings.TrimSuffix(header.Name, "/")] = header
			}
			if len(headers) != 11 {
				t.Fatalf("got %d headers, want 11: %v", len(headers), headers)
			}
			for name, want := range map[string]struct {
				mode     int64
				uid, gid int
			}{
				"app":          {0o755, 0, 0},
				"app/first":    {0o755, 0, 0},
				"app/second":   {0o755, 0, 0},
				"app/first/a":  {0o644, 42, 43},
				"app/first/b":  {0o600, 42, 43},
				"app/first/c":  {0o644, 42, 43},
				"app/second/a": {0o700, 42, 43},
				"app/second/b": {0o644, 42, 43},
				"app/second/c": {0o644, 42, 43},
				"links":        {0o750, 44, 0},
				"links/alias":  {0o777, 42, 43},
			} {
				h := headers[name]
				if h == nil {
					t.Fatalf("missing %s", name)
				}
				if h.Mode != want.mode || h.Uid != want.uid || h.Gid != want.gid || !h.ModTime.Equal(time.Date(2023, 1, 1, 8, 0, 0, 0, time.UTC)) {
					t.Errorf("%s: mode=%o uid=%d gid=%d mtime=%s", name, h.Mode, h.Uid, h.Gid, h.ModTime)
				}
			}
			for _, name := range []string{"app/first/a", "app/first/b", "app/second/a"} {
				if headers[name].Typeflag != tar.TypeReg {
					t.Errorf("%s: differently permissioned files must not be hardlinked", name)
				}
			}
			if h := headers["app/first/c"]; h.Typeflag != tar.TypeLink || h.Linkname != "app/first/a" {
				t.Errorf("identical content and metadata were not deduplicated: %+v", h)
			}
			if h := headers["links/alias"]; h.Typeflag != tar.TypeSymlink || h.Linkname != "../app/first/a" {
				t.Errorf("symlink changed: %+v", h)
			}
		})
	}
}

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
