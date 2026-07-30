package deploy

import (
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/progress"
)

func TestExtractProgressFlag(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "absent", args: []string{"--request-file", "req.json"}, want: ""},
		{name: "joined", args: []string{"--progress=log", "--jobs=4"}, want: "log"},
		{name: "separate", args: []string{"--progress", "none"}, want: "none"},
		{name: "missing value", args: []string{"--jobs=4", "--progress"}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractProgressFlag(tc.args); got != tc.want {
				t.Errorf("extractProgressFlag(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

func TestApplyProgressMode(t *testing.T) {
	// Auto-detection depends on the environment, so only explicit values are
	// asserted here (see the progress package for the auto-detection tests).
	for _, tc := range []struct {
		name             string
		value            string
		persistentWorker bool
		want             progress.Mode
	}{
		{name: "bar", value: "bar", want: progress.ModeBar},
		{name: "log", value: "log", want: progress.ModeLog},
		{name: "none", value: "none", want: progress.ModeNone},
		{
			// Bars can't work when requests are multiplexed onto one stderr.
			name:             "bar degrades to log in the worker",
			value:            "bar",
			persistentWorker: true,
			want:             progress.ModeLog,
		},
		{
			name:             "none is honored in the worker",
			value:            "none",
			persistentWorker: true,
			want:             progress.ModeNone,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyProgressMode(tc.value, tc.persistentWorker); err != nil {
				t.Fatalf("applyProgressMode(%q, %v) returned error: %v", tc.value, tc.persistentWorker, err)
			}
			if got := progress.CurrentMode(); got != tc.want {
				t.Errorf("progress mode = %v, want %v", got, tc.want)
			}
		})
	}

	if err := applyProgressMode("bogus", false); err == nil {
		t.Error("applyProgressMode(\"bogus\", false) = nil error, want an error")
	}
}
